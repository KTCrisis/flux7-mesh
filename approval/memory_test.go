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
		if args["agent"] != "agent-mesh" {
			t.Fatalf("expected agent=agent-mesh, got %v", args["agent"])
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
