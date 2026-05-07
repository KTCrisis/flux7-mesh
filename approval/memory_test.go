package approval

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		if value == "" || value[0:8] != "rejected" {
			t.Fatalf("expected value to start with 'rejected', got %q", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for memory write")
	}
}
