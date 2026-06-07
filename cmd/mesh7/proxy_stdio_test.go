package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDaemonRunningNoServer(t *testing.T) {
	if daemonRunning(19999) {
		t.Error("expected false when no server is listening")
	}
}

func TestDaemonRunningWithServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	var port int
	fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	if !daemonRunning(port) {
		t.Error("expected true when server is listening")
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
