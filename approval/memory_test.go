package approval

import (
	"encoding/json"
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

func TestCountDecisions(t *testing.T) {
	tests := []struct {
		name             string
		text             string
		wantApproved     int
		wantRejected     int
	}{
		{
			"empty",
			"",
			0, 0,
		},
		{
			"three approvals",
			"approved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read\napproved by supervisor:auto — agent:claude tool:fs.read",
			3, 0,
		},
		{
			"mixed",
			"approved by user:marc — agent:claude tool:fs.read\nrejected by user:marc — agent:claude tool:fs.write\napproved by user:marc — agent:claude tool:fs.read",
			2, 1,
		},
		{
			"no match lines",
			"Found 2 memories matching \"fs.read\":\n\n[1] decision.fs.read.abc\napproved by user:marc — agent:claude tool:fs.read\nTags: decision, approved\n---",
			1, 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, r := countDecisions(tt.text)
			if a != tt.wantApproved || r != tt.wantRejected {
				t.Errorf("countDecisions() = (%d, %d), want (%d, %d)", a, r, tt.wantApproved, tt.wantRejected)
			}
		})
	}
}

func TestAutoResolveApprove(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"approved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read"}]}}`
		w.Write([]byte(resp))
	}))
	defer srv.Close()

	mr := NewMemoryReader(srv.URL, "", 3)
	result := mr.AutoResolve("fs.read", "claude")

	if result.Action != "approve" {
		t.Fatalf("expected approve, got %s: %s", result.Action, result.Reason)
	}
	if result.Approved != 3 || result.Rejected != 0 {
		t.Fatalf("counts wrong: approved=%d rejected=%d", result.Approved, result.Rejected)
	}
	if result.Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9, got %f", result.Confidence)
	}
}

func TestAutoResolveEscalateNotEnough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"approved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read"}]}}`
		w.Write([]byte(resp))
	}))
	defer srv.Close()

	mr := NewMemoryReader(srv.URL, "", 3)
	result := mr.AutoResolve("fs.read", "claude")

	if result.Action != "escalate" {
		t.Fatalf("expected escalate, got %s", result.Action)
	}
	if result.Approved != 2 {
		t.Fatalf("expected 2 approvals, got %d", result.Approved)
	}
}

func TestAutoResolveEscalateRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"approved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read\nrejected by user:marc — agent:claude tool:fs.read"}]}}`
		w.Write([]byte(resp))
	}))
	defer srv.Close()

	mr := NewMemoryReader(srv.URL, "", 3)
	result := mr.AutoResolve("fs.read", "claude")

	if result.Action != "escalate" {
		t.Fatalf("expected escalate with rejections, got %s", result.Action)
	}
	if result.Rejected != 1 {
		t.Fatalf("expected 1 rejection, got %d", result.Rejected)
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

func TestAutoResolveAuthToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":""}]}}`))
	}))
	defer srv.Close()

	mr := NewMemoryReader(srv.URL, "secret-token", 3)
	mr.AutoResolve("fs.read", "claude")

	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected auth header 'Bearer secret-token', got %q", gotAuth)
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
	// mem7 search returns 3 approvals
	searchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		params := payload["params"].(map[string]any)

		if params["name"] == "memory_search" {
			resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"approved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read\napproved by user:marc — agent:claude tool:fs.read"}]}}`
			w.Write([]byte(resp))
		} else if params["name"] == "memory_store" {
			// Auto-approval decision write
			w.WriteHeader(200)
			w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
		}
	}))
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
