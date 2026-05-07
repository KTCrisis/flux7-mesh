package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// MemoryWriter persists approval decisions to a mem7 server via JSON-RPC.
// All writes are fire-and-forget — a failing mem7 never blocks approvals.
type MemoryWriter struct {
	client *http.Client
	url    string
	token  string
	reqID  int
}

// NewMemoryWriter creates a writer. url is the mem7 base URL (e.g. "http://localhost:9070").
func NewMemoryWriter(url, token string) *MemoryWriter {
	return &MemoryWriter{
		client: &http.Client{Timeout: 5 * time.Second},
		url:    url,
		token:  token,
	}
}

// WriteDecision stores an approval decision as a fact in mem7.
func (m *MemoryWriter) WriteDecision(pa *PendingApproval, res Resolution) {
	if m == nil || m.url == "" {
		return
	}

	action := "approved"
	if res.Status == StatusDenied {
		action = "rejected"
	} else if res.Status == StatusTimeout {
		action = "timed out"
	}

	key := fmt.Sprintf("decision.%s.%s", pa.Tool, pa.ID)
	value := fmt.Sprintf("%s by %s — agent:%s tool:%s",
		action, res.ResolvedBy, pa.AgentID, pa.Tool)
	if res.Reasoning != "" {
		value += " reason:" + res.Reasoning
	}

	tags := []string{"decision", string(res.Status), pa.Tool}
	if pa.AgentID != "" {
		tags = append(tags, "agent:"+pa.AgentID)
	}

	go m.store(key, value, tags)
}

func (m *MemoryWriter) store(key, value string, tags []string) {
	m.reqID++
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      m.reqID,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "memory_store",
			"arguments": map[string]any{
				"key":   key,
				"value": value,
				"tags":  tags,
				"agent": "agent-mesh",
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("memory writer marshal failed", "error", err)
		return
	}

	req, err := http.NewRequest("POST", m.url+"/rpc", bytes.NewReader(body))
	if err != nil {
		slog.Error("memory writer request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		slog.Warn("memory writer failed", "url", m.url, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("memory writer got error status", "url", m.url, "status", resp.StatusCode)
	}
}
