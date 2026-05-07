package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// MemoryWriter persists approval decisions to a mem7 server via JSON-RPC.
// All writes are fire-and-forget — a failing mem7 never blocks approvals.
//
// Counters (attempted/succeeded/failed) are exposed via Stats() for the
// /metrics endpoint. They allow operators to detect a silent mem7 outage,
// which would otherwise degrade Phase 2 supervisor decisions invisibly.
type MemoryWriter struct {
	client *http.Client
	url    string
	token  string
	reqID  atomic.Int64

	attempted atomic.Int64
	succeeded atomic.Int64
	failed    atomic.Int64
}

// MemoryWriterStats is a point-in-time snapshot of mem7 write counters.
type MemoryWriterStats struct {
	Attempted int64
	Succeeded int64
	Failed    int64
}

// Stats returns a snapshot of mem7 write counters. Safe on a nil receiver
// and on a writer with no URL configured (returns zero values).
func (m *MemoryWriter) Stats() MemoryWriterStats {
	if m == nil {
		return MemoryWriterStats{}
	}
	return MemoryWriterStats{
		Attempted: m.attempted.Load(),
		Succeeded: m.succeeded.Load(),
		Failed:    m.failed.Load(),
	}
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
//
// Provenance convention: agent-mesh is recorded as the writer of the fact
// (mem7 `agent` field, hardcoded below), not as its subject. The originating
// agent that triggered the approval is preserved in the tags as
// "agent:<id>". agent-mesh is the witness; the worker agent is the actor.
// This keeps cross-agent provenance queryable while preserving an honest
// who-wrote-what audit trail — a future supervisor reading these decisions
// must filter by the "agent:<id>" tag, not by the writer.
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
	m.attempted.Add(1)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      m.reqID.Add(1),
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
		m.failed.Add(1)
		slog.Warn("memory writer marshal failed", "error", err)
		return
	}

	req, err := http.NewRequest("POST", m.url+"/rpc", bytes.NewReader(body))
	if err != nil {
		m.failed.Add(1)
		slog.Warn("memory writer request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		m.failed.Add(1)
		slog.Warn("memory writer failed", "url", m.url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		m.failed.Add(1)
		slog.Warn("memory writer got error status", "url", m.url, "status", resp.StatusCode)
		return
	}

	m.succeeded.Add(1)
}
