package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
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
	Action     string // "approve" or "escalate"
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

// AutoResolve counts the precedents of exactly this tool and agent and
// returns "approve" when the pattern is clear (>= minApprovals human
// approvals, no refusal), or "escalate" to defer to human/external supervisor.
//
// Only human approvals count: an approval by a supervisor (sup7) or by
// auto-approval itself (supervisor:mem7) is not a precedent, or an automatic
// decision would feed the next one and grow without a human (seen on
// 2026-09-29: "3 prior approvals", then 4, 5, 6). A refusal from anyone
// blocks. Facts are selected by exact tags (memory_list: every tag must
// match), not by semantic search, so another tool's decisions never count
// and no refusal is lost past a result limit.
func (m *MemoryReader) AutoResolve(tool, agentID string) AutoResolveResult {
	if m == nil || m.url == "" {
		return AutoResolveResult{Action: "escalate", Reason: "no memory server"}
	}
	scope := []string{"decision", tool}
	if agentID != "" {
		scope = append(scope, "agent:"+agentID)
	}
	approved, err := m.count(append([]string{"approved", "by:human"}, scope...))
	if err == nil {
		var rejected int
		rejected, err = m.count(append([]string{"denied"}, scope...))
		if err == nil {
			return m.verdict(approved, rejected)
		}
	}
	slog.Warn("mem7 query failed, escalating", "tool", tool, "agent", agentID, "error", err)
	return AutoResolveResult{Action: "escalate", Reason: "mem7 query failed"}
}

func (m *MemoryReader) verdict(approved, rejected int) AutoResolveResult {
	switch {
	case rejected > 0:
		return AutoResolveResult{Action: "escalate", Approved: approved, Rejected: rejected,
			Reason: fmt.Sprintf("mem7: %d human approvals, %d refusals — escalating", approved, rejected)}
	case approved >= m.minApprovals:
		return AutoResolveResult{Action: "approve", Confidence: 0.9, Approved: approved,
			Reason: fmt.Sprintf("mem7: %d prior human approvals, 0 refusals", approved)}
	default:
		return AutoResolveResult{Action: "escalate", Approved: approved,
			Reason: fmt.Sprintf("mem7: %d human approvals (need %d) — escalating", approved, m.minApprovals)}
	}
}

// Precedents returns the counts AutoResolve decides on, for display.
func (m *MemoryReader) Precedents(tool, agentID string) (approved, rejected int, err error) {
	if m == nil || m.url == "" {
		return 0, 0, fmt.Errorf("no memory server")
	}
	scope := []string{"decision", tool}
	if agentID != "" {
		scope = append(scope, "agent:"+agentID)
	}
	if approved, err = m.count(append([]string{"approved", "by:human"}, scope...)); err != nil {
		return
	}
	rejected, err = m.count(append([]string{"denied"}, scope...))
	return
}

var listCount = regexp.MustCompile(`(?m)^(\d+) memor(?:y|ies):`)

// count asks mem7 for the facts carrying every tag and returns how many.
func (m *MemoryReader) count(tags []string) (int, error) {
	text, err := m.call("memory_list", map[string]any{"tags": tags})
	if err != nil {
		return 0, err
	}
	if match := listCount.FindStringSubmatch(text); match != nil {
		return strconv.Atoi(match[1])
	}
	if strings.Contains(text, "No memories found") || strings.TrimSpace(text) == "" {
		return 0, nil
	}
	return 0, fmt.Errorf("unexpected memory_list answer: %.80q", text)
}

func (m *MemoryReader) call(tool string, arguments map[string]any) (string, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      m.reqID.Add(1),
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": arguments},
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

// WriteDecision stores an approval decision as a fact in mem7.
//
// Provenance convention: flux7-mesh is recorded as the writer of the fact
// (mem7 `agent` field, hardcoded below), not as its subject. The originating
// agent that triggered the approval is preserved in the tags as
// "agent:<id>". flux7-mesh is the witness; the worker agent is the actor.
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

	tags := []string{"decision", string(res.Status), pa.Tool, "by:" + resolverKind(res.ResolvedBy)}
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
				"agent": "flux7-mesh",
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

// resolverKind tags who settled a decision, so precedents can be restricted
// to human ones: human, supervisor (sup7), mem7 (auto-approval) or system.
func resolverKind(resolvedBy string) string {
	v := strings.ToLower(resolvedBy)
	switch {
	case v == "supervisor:mem7":
		return "mem7"
	case strings.HasPrefix(v, "supervisor:"), strings.HasPrefix(v, "auto:"), strings.HasPrefix(v, "bot:"):
		return "supervisor"
	case strings.HasPrefix(v, "system:"):
		return "system"
	default:
		return "human"
	}
}

// Precedent sums up the decisions mem7 holds for one tool and agent.
type Precedent struct {
	Tool             string `json:"tool"`
	Agent            string `json:"agent"`
	HumanApproved    int    `json:"human_approved"`     // the only approvals that count
	OtherApproved    int    `json:"other_approved"`     // by sup7 or by auto-approval: never counted
	Refused          int    `json:"refused"`            // by anyone: one blocks
	Untagged         int    `json:"untagged"`           // written before the by: tag, not counted
	Last             string `json:"last"`               // most recent decision (RFC 3339)
	AutoApprovable   bool   `json:"auto_approvable"`    // the tool may be approved from precedents at all
	WouldAutoApprove bool   `json:"would_auto_approve"` // the next call would pass without the supervisor
}

// MinApprovals is the number of human approvals a precedent needs.
func (m *MemoryReader) MinApprovals() int {
	if m == nil {
		return 0
	}
	return m.minApprovals
}

var listLine = regexp.MustCompile(`^- decision\.(\S+)\.[0-9a-f]+ \[([^\]]*)\].*— (\S+)$`)

// ListPrecedents reads every decision fact and groups it by tool and agent.
func (m *MemoryReader) ListPrecedents() ([]Precedent, error) {
	if m == nil || m.url == "" {
		return nil, fmt.Errorf("no memory server")
	}
	text, err := m.call("memory_list", map[string]any{"tags": []string{"decision"}})
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Precedent{}
	var order []string
	for _, line := range strings.Split(text, "\n") {
		match := listLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		tool, at := match[1], match[3]
		var status, by, agent string
		for _, tag := range strings.Split(match[2], ",") {
			tag = strings.TrimSpace(tag)
			switch {
			case tag == "approved" || tag == "denied" || tag == "timeout":
				status = tag
			case strings.HasPrefix(tag, "by:"):
				by = strings.TrimPrefix(tag, "by:")
			case strings.HasPrefix(tag, "agent:"):
				agent = strings.TrimPrefix(tag, "agent:")
			}
		}
		key := tool + "\x00" + agent
		p := byKey[key]
		if p == nil {
			p = &Precedent{Tool: tool, Agent: agent}
			byKey[key] = p
			order = append(order, key)
		}
		switch {
		case status == "denied":
			p.Refused++
		case status == "approved" && by == "human":
			p.HumanApproved++
		case status == "approved" && by == "":
			p.Untagged++
		case status == "approved":
			p.OtherApproved++
		}
		if at > p.Last {
			p.Last = at
		}
	}
	out := make([]Precedent, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out, nil
}

// ForgetPrecedents removes every decision of one tool and agent from mem7:
// the next calls start again from no precedent.
func (m *MemoryReader) ForgetPrecedents(tool, agentID string) (string, error) {
	if m == nil || m.url == "" {
		return "", fmt.Errorf("no memory server")
	}
	if tool == "" || agentID == "" {
		return "", fmt.Errorf("tool and agent are required")
	}
	return m.call("memory_forget", map[string]any{"tags": []string{"decision", tool, "agent:" + agentID}})
}
