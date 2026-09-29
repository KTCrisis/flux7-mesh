package approval

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryWriterWriteDecision(t *testing.T) {
	received := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		received <- payload

		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
		}

		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"text":"ok"}]}}`))
	}))
	defer srv.Close()

	mw := NewMemoryWriter(srv.URL, "test-token")
	pa := &PendingApproval{
		ID:      "abc123",
		AgentID: "claude",
		Tool:    "gmail.send_email",
	}
	res := Resolution{
		Status:     StatusApproved,
		ResolvedBy: "user:marc",
		ResolvedAt: time.Now(),
		Reasoning:  "routine send",
	}

	mw.WriteDecision(pa, res)

	select {
	case payload := <-received:
		params := payload["params"].(map[string]any)
		if params["name"] != "memory_store" {
			t.Fatalf("expected memory_store, got %v", params["name"])
		}
		args := params["arguments"].(map[string]any)
		if args["key"] != "decision.gmail.send_email.abc123" {
			t.Fatalf("unexpected key: %v", args["key"])
		}
		value := args["value"].(string)
		if value == "" {
			t.Fatal("expected non-empty value")
		}
		tags := args["tags"].([]any)
		if len(tags) < 3 {
			t.Fatalf("expected at least 3 tags, got %d", len(tags))
		}
		if args["agent"] != "flux7-mesh" {
			t.Fatalf("expected agent=flux7-mesh, got %v", args["agent"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for memory write")
	}
}

func TestMemoryWriterNilSafe(t *testing.T) {
	var mw *MemoryWriter
	mw.WriteDecision(&PendingApproval{}, Resolution{})
}

func TestMemoryWriterEmptyURL(t *testing.T) {
	mw := NewMemoryWriter("", "")
	mw.WriteDecision(&PendingApproval{}, Resolution{})
}

func TestMemoryWriterStatsNilSafe(t *testing.T) {
	var mw *MemoryWriter
	stats := mw.Stats()
	if stats.Attempted != 0 || stats.Succeeded != 0 || stats.Failed != 0 {
		t.Fatalf("expected zero stats on nil writer, got %+v", stats)
	}
}

func TestMemoryWriterStatsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	mw := NewMemoryWriter(srv.URL, "")
	pa := &PendingApproval{ID: "1", AgentID: "claude", Tool: "fs.read"}
	res := Resolution{Status: StatusApproved, ResolvedBy: "user:marc"}

	mw.WriteDecision(pa, res)
	mw.WriteDecision(pa, res)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := mw.Stats()
		if s.Attempted == 2 && s.Succeeded == 2 && s.Failed == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats did not converge: %+v", mw.Stats())
}

func TestMemoryWriterStatsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	mw := NewMemoryWriter(srv.URL, "")
	pa := &PendingApproval{ID: "1", AgentID: "claude", Tool: "fs.read"}
	res := Resolution{Status: StatusApproved, ResolvedBy: "user:marc"}

	mw.WriteDecision(pa, res)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := mw.Stats()
		if s.Attempted == 1 && s.Succeeded == 0 && s.Failed == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats did not converge: %+v", mw.Stats())
}

// --- MemoryReader tests ---

// listServer plays mem7 for precedents: memory_list answers the given number
// of approved (by:human) and denied facts, and records the tags it was asked.
func listServer(t *testing.T, approved, denied int, asked *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		params := payload["params"].(map[string]any)
		if params["name"] != "memory_list" {
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		var tags []string
		for _, x := range params["arguments"].(map[string]any)["tags"].([]any) {
			tags = append(tags, x.(string))
		}
		if asked != nil {
			*asked = append(*asked, tags)
		}
		n := 0
		switch {
		case contains(tags, "approved") && contains(tags, "by:human"):
			n = approved
		case contains(tags, "denied"):
			n = denied
		}
		text := "No memories found."
		if n > 0 {
			text = fmt.Sprintf("%d memories:\n", n)
			for i := 0; i < n; i++ {
				text += fmt.Sprintf("- decision.fs.read.%d [decision] (by flux7-mesh)\n", i)
			}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
		w.Write(resp)
	}))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestAutoResolveApprove(t *testing.T) {
	var asked [][]string
	srv := listServer(t, 3, 0, &asked)
	defer srv.Close()

	result := NewMemoryReader(srv.URL, "", 3).AutoResolve("fs.read", "claude")
	if result.Action != "approve" || result.Approved != 3 || result.Confidence != 0.9 {
		t.Fatalf("expected approve on 3 human approvals, got %+v", result)
	}
	// exact scope: this tool, this agent, human approvals; refusals from anyone
	want := [][]string{
		{"approved", "by:human", "decision", "fs.read", "agent:claude"},
		{"denied", "decision", "fs.read", "agent:claude"},
	}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("mem7 asked %v, want %v", asked, want)
	}
}

func TestAutoResolveEscalateNotEnough(t *testing.T) {
	srv := listServer(t, 2, 0, nil)
	defer srv.Close()
	result := NewMemoryReader(srv.URL, "", 3).AutoResolve("fs.read", "claude")
	if result.Action != "escalate" || result.Approved != 2 {
		t.Fatalf("expected escalate on 2 approvals, got %+v", result)
	}
}

func TestAutoResolveEscalateRejections(t *testing.T) {
	srv := listServer(t, 5, 1, nil)
	defer srv.Close()
	result := NewMemoryReader(srv.URL, "", 3).AutoResolve("fs.read", "claude")
	if result.Action != "escalate" || result.Rejected != 1 {
		t.Fatalf("one refusal must block, got %+v", result)
	}
}

func TestResolverKindTagsWhoDecided(t *testing.T) {
	for by, want := range map[string]string{
		"user:marc": "human", "http:127.0.0.1:5000": "human", "cli": "human",
		"supervisor:supervisor": "supervisor", "supervisor:mem7": "mem7", "system:timeout": "system",
	} {
		if got := resolverKind(by); got != want {
			t.Errorf("resolverKind(%q) = %q, want %q", by, got, want)
		}
	}
}

func TestAutoResolveMem7Answer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"something else"}]}}`))
	}))
	defer srv.Close()
	if r := NewMemoryReader(srv.URL, "", 3).AutoResolve("fs.read", "claude"); r.Action != "escalate" {
		t.Fatalf("an unreadable mem7 answer must escalate, got %+v", r)
	}
}

func TestAutoResolveAuthToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"No memories found."}]}}`))
	}))
	defer srv.Close()

	NewMemoryReader(srv.URL, "secret-token", 3).AutoResolve("fs.read", "claude")
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected auth header 'Bearer secret-token', got %q", gotAuth)
	}
}

func TestAutoResolveNilSafe(t *testing.T) {
	var mr *MemoryReader
	result := mr.AutoResolve("fs.read", "claude")
	if result.Action != "escalate" {
		t.Fatalf("expected escalate on nil reader, got %s", result.Action)
	}
}

func TestAutoResolveMem7Down(t *testing.T) {
	mr := NewMemoryReader("http://localhost:1", "", 3)
	result := mr.AutoResolve("fs.read", "claude")
	if result.Action != "escalate" {
		t.Fatalf("expected escalate on unreachable mem7, got %s", result.Action)
	}
}

func TestTryAutoResolveNilStore(t *testing.T) {
	var s *Store
	res := s.TryAutoResolve("claude", "fs.read")
	if res != nil {
		t.Fatal("expected nil on nil store")
	}
}

func TestTryAutoResolveNoReader(t *testing.T) {
	s := NewStore(5 * time.Minute)
	res := s.TryAutoResolve("claude", "fs.read")
	if res != nil {
		t.Fatal("expected nil when no reader configured")
	}
}

func TestTryAutoResolveWritesDecision(t *testing.T) {
	searchSrv := listServer(t, 3, 0, nil)
	defer searchSrv.Close()

	s := NewStore(5 * time.Minute)
	s.MemoryReader = NewMemoryReader(searchSrv.URL, "", 3)
	s.MemoryWriter = NewMemoryWriter(searchSrv.URL, "")

	res := s.TryAutoResolve("claude", "fs.read")
	if res == nil {
		t.Fatal("expected auto-resolve result")
	}
	if res.Status != StatusApproved {
		t.Fatalf("expected approved, got %s", res.Status)
	}
	if res.ResolvedBy != "supervisor:mem7" {
		t.Fatalf("expected supervisor:mem7, got %s", res.ResolvedBy)
	}
}

func TestMemoryWriterDeniedDecision(t *testing.T) {
	received := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		received <- payload
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mw := NewMemoryWriter(srv.URL, "")
	pa := &PendingApproval{
		ID:      "def456",
		AgentID: "scout7",
		Tool:    "filesystem.write_file",
	}
	res := Resolution{
		Status:     StatusDenied,
		ResolvedBy: "supervisor:auto",
	}

	mw.WriteDecision(pa, res)

	select {
	case payload := <-received:
		params := payload["params"].(map[string]any)
		args := params["arguments"].(map[string]any)
		value := args["value"].(string)
		if !strings.HasPrefix(value, "rejected") {
			t.Fatalf("expected value to start with 'rejected', got %q", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for memory write")
	}
}

// Precedents approve only what AutoApprovable allows: a write with three
// approved precedents still goes to the supervisor, and mem7 is not asked.
func TestAutoApprovableGatesPrecedents(t *testing.T) {
	var asked [][]string
	srv := listServer(t, 3, 0, &asked)
	defer srv.Close()

	s := NewStore(time.Minute)
	s.MemoryReader = NewMemoryReader(srv.URL, "", 3)
	s.AutoApprovable = func(tool string) bool { return tool == "fs.read" }

	if res := s.TryAutoResolveSafe("claude", "fs.write", map[string]any{"path": "/home/u/.bashrc"}); res != nil {
		t.Fatalf("a write was auto-approved from precedents: %+v", res)
	}
	if len(asked) != 0 {
		t.Fatalf("mem7 was queried %d times for a tool that cannot be auto-approved", len(asked))
	}
	if res := s.TryAutoResolveSafe("claude", "fs.read", map[string]any{"path": "/x"}); res == nil {
		t.Fatal("a read with three approved precedents was not auto-approved")
	}
}
