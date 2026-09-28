package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
