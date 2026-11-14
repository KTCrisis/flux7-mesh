package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemonOnPortNoServer(t *testing.T) {
	if running, _ := daemonOnPort(19999); running {
		t.Error("expected false when no server is listening")
	}
}

func TestDaemonOnPortReportsConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","config":"abc123"}`))
	}))
	defer srv.Close()

	var port int
	fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	running, cfg := daemonOnPort(port)
	if !running || cfg != "abc123" {
		t.Errorf("daemonOnPort = %v, %q; want true, abc123", running, cfg)
	}
}

func TestConfigIDSamePathSameID(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "config.yaml")
	os.WriteFile(a, []byte("port: 1\n"), 0o644)
	link := filepath.Join(dir, "link.yaml")
	os.Symlink(a, link)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)

	if configID(a) != configID("config.yaml") {
		t.Error("relative and absolute paths to one file must give one ID")
	}
	if configID(a) != configID(link) {
		t.Error("a symlink to the file must give the same ID")
	}
	if configID(a) == configID(filepath.Join(dir, "other.yaml")) {
		t.Error("two files must give two IDs")
	}
	if strings.Contains(configID(a), "config") {
		t.Error("the ID must not disclose the path")
	}
}

func TestProxyDecision(t *testing.T) {
	if ok, warn, err := proxyDecision("x", "x", 9090); !ok || warn != "" || err != nil {
		t.Errorf("same config: ok=%v warn=%q err=%v", ok, warn, err)
	}
	if ok, _, err := proxyDecision("x", "y", 9090); ok || err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("other config must be refused with a way out: ok=%v err=%v", ok, err)
	}
	if ok, warn, err := proxyDecision("x", "", 9090); !ok || warn == "" || err != nil {
		t.Errorf("older daemon: relay with a warning, got ok=%v warn=%q err=%v", ok, warn, err)
	}
}

func TestStdioProxyForward(t *testing.T) {
	var gotSessionID string
	var gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gotSessionID = r.Header.Get("Mcp-Session-Id")
		gotAgent = r.Header.Get("Authorization")

		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Mcp-Session-Id", "sess-123")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  map[string]any{"ok": true},
		})
	}))
	defer srv.Close()

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"

	var out bytes.Buffer
	err := runStdioProxyWith(srv.URL, "claude", "", strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}

	if gotAgent != "Bearer agent:claude" {
		t.Errorf("expected agent header, got %s", gotAgent)
	}
	if gotSessionID != "sess-123" {
		t.Errorf("second request should forward session ID, got %s", gotSessionID)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 response lines, got %d: %v", len(lines), lines)
	}
}

func TestStdioProxyNotificationSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "sess-456")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	input := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"

	var out bytes.Buffer
	err := runStdioProxyWith(srv.URL, "claude", "", strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for notification, got %q", out.String())
	}
}

func TestStdioProxyUsesTokenWhenProvided(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
	}))
	defer srv.Close()

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	var out bytes.Buffer
	// A JWT-like token must be presented verbatim, not wrapped in agent:.
	if err := runStdioProxyWith(srv.URL, "claude", "header.payload.sig", strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer header.payload.sig" {
		t.Errorf("expected raw token bearer, got %q", gotAuth)
	}
}

// syncBuffer is a bytes.Buffer two goroutines can use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The daemon cannot push; the proxy tells the client when the catalogue moved,
// and only once the client has finished its handshake.
func TestStdioProxyAnnouncesCatalogChanges(t *testing.T) {
	old := catalogPollInterval
	catalogPollInterval = 20 * time.Millisecond
	defer func() { catalogPollInterval = old }()

	var catalog atomic.Uint64
	catalog.Store(4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprintf(w, `{"status":"ok","catalog":%d}`, catalog.Load())
			return
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"notifications/`)) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	in, feed := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runStdioProxyWith(srv.URL, "claude", "", in, out) }()
	const note = `"notifications/tools/list_changed"`
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("never happened: %s; out = %q", what, out.String())
	}

	// Before the handshake is over, a change is not announced.
	fmt.Fprintln(feed, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	wait("the initialize answer", func() bool { return strings.Contains(out.String(), `"id":1`) })
	catalog.Store(5)
	time.Sleep(120 * time.Millisecond)
	if strings.Contains(out.String(), note) {
		t.Fatal("list_changed sent before notifications/initialized")
	}

	fmt.Fprintln(feed, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(60 * time.Millisecond)
	if strings.Contains(out.String(), note) {
		t.Fatal("list_changed sent although the catalogue did not move since the handshake")
	}
	catalog.Store(6)
	wait("list_changed after a catalogue change", func() bool { return strings.Count(out.String(), note) == 1 })

	// One change, one line, and each line is whole JSON.
	time.Sleep(100 * time.Millisecond)
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("interleaved output line: %q", line)
		}
	}
	if n := strings.Count(out.String(), note); n != 1 {
		t.Errorf("%d notifications for one change", n)
	}

	feed.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The bridge names its conversation to the daemon: MESH7_SESSION_LABEL first,
// else the session id Claude Code gives its MCP servers.
func TestStdioProxySendsClientSessionLabel(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Session-Id"))
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
	}))
	defer srv.Close()
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"

	t.Setenv("MESH7_SESSION_LABEL", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "claude-session-1")
	var out bytes.Buffer
	if err := runStdioProxyWith(srv.URL, "claude", "", strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH7_SESSION_LABEL", "my-run")
	if err := runStdioProxyWith(srv.URL, "claude", "", strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "claude-session-1" || got[1] != "my-run" {
		t.Errorf("X-Session-Id sent = %v, want [claude-session-1 my-run]", got)
	}
}

// Once the session exists, a slow call no longer holds the others: the fast
// one is answered while the slow one is still running.
func TestStdioProxyCallsRunInParallel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-par")
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Params.Name == "slow" {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"name": req.Params.Name}})
	}))
	defer srv.Close()

	in, feed := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runStdioProxyWith(srv.URL, "claude", "", in, out) }()

	fmt.Fprintln(feed, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	fmt.Fprintln(feed, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	fmt.Fprintln(feed, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow"}}`)
	fmt.Fprintln(feed, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fast"}}`)

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), `"id":3`) {
		if time.Now().After(deadline) {
			t.Fatalf("the fast call waited behind the slow one; out = %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(out.String(), `"id":2`) {
		t.Fatal("the slow call answered before it was released")
	}
	close(release)
	feed.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// the bridge waits for the call still in flight before it returns
	if !strings.Contains(out.String(), `"id":2`) {
		t.Errorf("the slow call's answer was lost; out = %q", out.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("interleaved output line: %q", line)
		}
	}
}

// A daemon that goes away during a parallel call: the caller still gets an
// answer for its id, and the bridge reports the failure.
func TestStdioProxyAnswersAFailedParallelCall(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/call" {
			// cut the connection without an answer
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-gone")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
	}))
	defer srv.Close()

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
		`{"jsonrpc":"2.0","id":"call-7","method":"tools/call","params":{"name":"x"}}` + "\n"
	out := &syncBuffer{}
	err := runStdioProxyWith(srv.URL, "claude", "", strings.NewReader(input), out)
	if err == nil {
		t.Error("a lost daemon should end the bridge with an error")
	}
	if !strings.Contains(out.String(), `"id":"call-7"`) || !strings.Contains(out.String(), `-32603`) {
		t.Errorf("no JSON-RPC error for the failed call; out = %q", out.String())
	}
}

func TestBridgeParallel(t *testing.T) {
	t.Setenv("MESH7_BRIDGE_PARALLEL", "")
	if bridgeParallel() != 8 {
		t.Errorf("default = %d, want 8", bridgeParallel())
	}
	t.Setenv("MESH7_BRIDGE_PARALLEL", "2")
	if bridgeParallel() != 2 {
		t.Errorf("MESH7_BRIDGE_PARALLEL=2 gives %d", bridgeParallel())
	}
	t.Setenv("MESH7_BRIDGE_PARALLEL", "zero")
	if bridgeParallel() != 8 {
		t.Errorf("a bad value should fall back to 8, got %d", bridgeParallel())
	}
}
