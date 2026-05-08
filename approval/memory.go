package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

// MemoryReader queries a mem7 server for past approval decisions.
// Used by the built-in supervisor (Level 1) to auto-approve routine patterns
// based on historical decisions stored by MemoryWriter.
type MemoryReader struct {
	client       *http.Client
	url          string
	token        string
	reqID        atomic.Int64
	minApprovals int
}

// AutoResolveResult is the outcome of checking mem7 for past decisions.
type AutoResolveResult struct {
	Action     string  // "approve" or "escalate"
	Reason     string
	Confidence float64
	Approved   int
	Rejected   int
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

// NewMemoryReader creates a reader. minApprovals is the threshold for
// auto-approving (default 3 if <= 0).
func NewMemoryReader(url, token string, minApprovals int) *MemoryReader {
	if minApprovals <= 0 {
		minApprovals = 3
	}
	return &MemoryReader{
		client:       &http.Client{Timeout: 3 * time.Second},
		url:          url,
		token:        token,
		minApprovals: minApprovals,
	}
}

// AutoResolve checks mem7 for past decisions matching tool+agent and returns
// "approve" if the pattern is clear (>= minApprovals with 0 rejections),
// or "escalate" to defer to human/external supervisor.
func (m *MemoryReader) AutoResolve(tool, agentID string) AutoResolveResult {
	if m == nil || m.url == "" {
		return AutoResolveResult{Action: "escalate", Reason: "no memory server"}
	}

	text, err := m.search(tool, agentID)
	if err != nil {
		slog.Warn("mem7 query failed, escalating", "tool", tool, "agent", agentID, "error", err)
		return AutoResolveResult{Action: "escalate", Reason: "mem7 query failed"}
	}

	approved, rejected := countDecisions(text)

	if approved >= m.minApprovals && rejected == 0 {
		return AutoResolveResult{
			Action:     "approve",
			Reason:     fmt.Sprintf("mem7: %d prior approvals, 0 rejections", approved),
			Confidence: 0.9,
			Approved:   approved,
			Rejected:   rejected,
		}
	}

	if rejected > 0 {
		return AutoResolveResult{
			Action:     "escalate",
			Reason:     fmt.Sprintf("mem7: %d approvals, %d rejections — escalating", approved, rejected),
			Approved:   approved,
			Rejected:   rejected,
		}
	}

	return AutoResolveResult{
		Action:   "escalate",
		Reason:   fmt.Sprintf("mem7: %d approvals (need %d) — escalating", approved, m.minApprovals),
		Approved: approved,
		Rejected: rejected,
	}
}

func (m *MemoryReader) search(tool, agentID string) (string, error) {
	query := tool
	if agentID != "" {
		query = tool + " " + agentID
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      m.reqID.Add(1),
		"method":  "tools/call",
		"params": map[string]any{
			"name": "memory_search",
			"arguments": map[string]any{
				"query": query,
				"tags":  []string{"decision"},
				"limit": 10,
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", m.url+"/rpc", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("mem7 returned status %d", resp.StatusCode)
	}

	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", err
	}

	for _, c := range rpcResp.Result.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", nil
}

// countDecisions parses mem7 search results and counts approval/rejection decisions.
// Matches the exact value format written by MemoryWriter.WriteDecision:
// "approved by X — agent:Y tool:Z" / "rejected by X — agent:Y tool:Z"
// Uses a strict prefix match to prevent poisoning via injected text.
func countDecisions(text string) (approved, rejected int) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "approved by ") && strings.Contains(lower, " — agent:") {
			approved++
		} else if strings.HasPrefix(lower, "rejected by ") && strings.Contains(lower, " — agent:") {
			rejected++
		}
	}
	return
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
