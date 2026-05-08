package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// doneChan wraps a channel with sync.Once to prevent double-close panics.
type doneChan struct {
	ch   chan struct{}
	once sync.Once
}

func newDoneChan() *doneChan {
	return &doneChan{ch: make(chan struct{})}
}

func (d *doneChan) Close() {
	d.once.Do(func() { close(d.ch) })
}

func (d *doneChan) Chan() <-chan struct{} {
	return d.ch
}

// MCPClient manages a connection to a single upstream MCP server.
type MCPClient struct {
	Name      string
	Transport string // "stdio" or "sse"
	Command   string // stdio: command (e.g. "npx", "/usr/bin/python")
	Args      []string // stdio: args
	URL       string // sse: endpoint URL

	// transport layer
	tr        transport
	newTr     func() transport // factory to create a fresh transport for reconnection

	// state
	stateMu   sync.Mutex
	nextID    atomic.Int64
	pending   map[int64]chan rpcResponse
	pendingMu sync.Mutex
	tools     []MCPTool
	status    string // "connecting", "ready", "error", "closed"
	lastError string
	done      *doneChan // closed when readLoop exits or client is closed
}

// NewStdioClient creates an MCP client that communicates via stdin/stdout of a subprocess.
func NewStdioClient(name, command string, args []string, env map[string]string) *MCPClient {
	factory := func() transport { return newStdioTransport(name, command, args, env) }
	return &MCPClient{
		Name:      name,
		Transport: "stdio",
		Command:   command,
		Args:      args,
		tr:        factory(),
		newTr:     factory,
		pending:   make(map[int64]chan rpcResponse),
		status:    "connecting",
		done:      newDoneChan(),
	}
}

// NewSSEClient creates an MCP client that communicates via HTTP SSE.
func NewSSEClient(name, sseURL string, headers map[string]string) *MCPClient {
	factory := func() transport { return newSSETransport(name, sseURL, headers) }
	return &MCPClient{
		Name:      name,
		Transport: "sse",
		URL:       sseURL,
		tr:        factory(),
		newTr:     factory,
		pending:   make(map[int64]chan rpcResponse),
		status:    "connecting",
		done:      newDoneChan(),
	}
}

// Connect starts the transport, performs the MCP initialize handshake, and discovers tools.
func (c *MCPClient) Connect(ctx context.Context) error {
	if err := c.connectInternal(ctx); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (c *MCPClient) connectInternal(ctx context.Context) error {
	if err := c.tr.Start(); err != nil {
		c.setStatus("error", err.Error())
		return fmt.Errorf("start transport: %w", err)
	}

	// Start read loop
	go c.readLoop()

	// Initialize handshake
	initResp, err := c.send(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "flux7-mesh",
			"version": "0.1.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	slog.Info("MCP client: initialized", "server", c.Name, "result", initResp.Result)

	// Send initialized notification (no response expected)
	if err := c.writeRequest(rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}); err != nil {
		return fmt.Errorf("send initialized notification: %w", err)
	}

	// Discover tools
	toolsResp, err := c.send(ctx, "tools/list", nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}

	if err := c.parseTools(toolsResp.Result); err != nil {
		return fmt.Errorf("parse tools: %w", err)
	}

	c.setStatus("ready", "")
	slog.Info("MCP client: ready", "server", c.Name, "tools", len(c.tools))
	return nil
}

// Tools returns the discovered MCP tools.
func (c *MCPClient) Tools() []MCPTool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	out := make([]MCPTool, len(c.tools))
	copy(out, c.tools)
	return out
}

// Status returns the current connection status.
func (c *MCPClient) Status() (status string, lastError string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.status, c.lastError
}

// CallTool invokes a tool on the upstream MCP server.
func (c *MCPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (any, error) {
	resp, err := c.send(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// Close shuts down the connection.
func (c *MCPClient) Close() error {
	c.setStatus("closed", "")
	c.failAllPending("client closed")
	c.done.Close()
	return c.tr.Close()
}

// send dispatches a JSON-RPC request and waits for the response.
func (c *MCPClient) send(ctx context.Context, method string, params map[string]any) (rpcResponse, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	if err := c.writeRequest(req); err != nil {
		return rpcResponse{}, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-c.done.Chan():
		return rpcResponse{}, fmt.Errorf("connection lost while waiting for %s", method)
	case <-ctx.Done():
		return rpcResponse{}, ctx.Err()
	}
}

func (c *MCPClient) writeRequest(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return c.tr.WriteRequest(data)
}

func (c *MCPClient) readLoop() {
	c.tr.ReadLoop(func(data []byte) {
		var resp rpcResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			slog.Warn("MCP client: invalid JSON from server", "server", c.Name, "error", err)
			return
		}

		if resp.ID == nil {
			return
		}
		id, ok := toInt64(resp.ID)
		if !ok {
			return
		}
		c.pendingMu.Lock()
		ch, found := c.pending[id]
		if found {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if found {
			ch <- resp
		}
	})

	// ReadLoop exited — try to reconnect unless intentionally closed
	status, _ := c.Status()
	if status == "closed" {
		return
	}
	c.setStatus("error", "connection lost")
	c.failAllPending("connection lost")

	// Attempt reconnection with exponential backoff
	go c.reconnectLoop()
}

// reconnectLoop attempts to reconnect with exponential backoff.
func (c *MCPClient) reconnectLoop() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for attempt := 1; ; attempt++ {
		status, _ := c.Status()
		if status == "closed" {
			return
		}

		slog.Info("MCP client: reconnecting", "server", c.Name, "attempt", attempt, "backoff", backoff)
		time.Sleep(backoff)

		// Check again after sleep
		status, _ = c.Status()
		if status == "closed" {
			return
		}

		// Create fresh transport and try to connect
		c.tr.Close()
		c.tr = c.newTr()
		c.done = newDoneChan()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := c.connectInternal(ctx)
		cancel()

		if err == nil {
			slog.Info("MCP client: reconnected", "server", c.Name, "attempt", attempt)
			return
		}

		slog.Warn("MCP client: reconnect failed", "server", c.Name, "attempt", attempt, "error", err)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// failAllPending drains all pending request channels with an error response.
func (c *MCPClient) failAllPending(reason string) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.pendingMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- rpcResponse{Error: &rpcError{Code: -1, Message: reason}}:
		default:
		}
	}
}

func (c *MCPClient) parseTools(result any) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var wrapper struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	c.stateMu.Lock()
	c.tools = wrapper.Tools
	c.stateMu.Unlock()
	return nil
}

func (c *MCPClient) setStatus(status, errMsg string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.status == "closed" {
		return
	}
	c.status = status
	c.lastError = errMsg
}

// helpers

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func trimBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func wait5s() <-chan time.Time {
	return time.After(5 * time.Second)
}
