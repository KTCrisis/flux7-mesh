package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collect drains the transport for the messages a test expects, with a
// deadline so a missing message fails loudly instead of hanging the suite.
func collect(t *testing.T, tr *streamableTransport, n int) []string {
	t.Helper()
	got := make([]string, 0, n)
	var mu sync.Mutex
	done := make(chan struct{})

	go tr.ReadLoop(func(b []byte) {
		mu.Lock()
		got = append(got, string(b))
		full := len(got) == n
		mu.Unlock()
		if full {
			close(done)
		}
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("expected %d messages, got %d: %v", n, len(got), got)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...)
}

func TestStreamableJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if a := r.Header.Get("Accept"); !strings.Contains(a, "application/json") || !strings.Contains(a, "text/event-stream") {
			t.Errorf("Accept must advertise both formats, got %q", a)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	if err := tr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer tr.Close()

	if err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := collect(t, tr, 1)
	if !strings.Contains(got[0], `"ok":true`) {
		t.Errorf("unexpected payload: %s", got[0])
	}
}

// A single request may be answered by several messages carried on an SSE
// stream; all of them must reach the client.
func TestStreamableSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"first\"}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":\"second\"}\n\n")
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	if err := tr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer tr.Close()

	if err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := collect(t, tr, 2)
	if !strings.Contains(got[0], "first") || !strings.Contains(got[1], "second") {
		t.Errorf("stream messages lost or reordered: %v", got)
	}
}

// The session id handed out on the first response must come back on every
// later request, and must not be sent before the server has issued one.
func TestStreamableSessionEcho(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()
		w.Header().Set("Mcp-Session-Id", "sess-42")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	tr.Start()
	defer tr.Close()

	for i := 0; i < 2; i++ {
		if err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	collect(t, tr, 2)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	if seen[0] != "" {
		t.Errorf("first request must not carry a session id, got %q", seen[0])
	}
	if seen[1] != "sess-42" {
		t.Errorf("second request must echo the session id, got %q", seen[1])
	}
}

func TestStreamableHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	tr.Start()
	defer tr.Close()

	err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should carry status and body, got: %v", err)
	}
}

// 202 with no body is the legal answer to a notification: no error, no message.
func TestStreamableAcceptedNotification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	tr.Start()
	defer tr.Close()

	if err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatalf("202 must not be an error: %v", err)
	}
}

// A redirect to another host would hand every tool call to a third party.
func TestStreamableRefusesCrossOriginRedirect(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must never reach the redirect target")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stolen":true}`)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, nil)
	tr.Start()
	defer tr.Close()

	err := tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil {
		t.Fatal("cross-origin redirect must be refused")
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("expected a cross-origin refusal, got: %v", err)
	}
}

func TestStreamableCustomHeaders(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	tr := newStreamableTransport("test", srv.URL, map[string]string{"Authorization": "Bearer hf_token"})
	tr.Start()
	defer tr.Close()

	tr.WriteRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	select {
	case h := <-got:
		if h != "Bearer hf_token" {
			t.Errorf("header not forwarded, got %q", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no request received")
	}
}

func TestStreamableRejectsNonHTTPEndpoint(t *testing.T) {
	for _, bad := range []string{"ftp://example.com/mcp", "not a url at all", ""} {
		tr := newStreamableTransport("test", bad, nil)
		if err := tr.Start(); err == nil {
			t.Errorf("endpoint %q should be rejected", bad)
		}
	}
}

// Close must release ReadLoop, otherwise a reconnect leaks a goroutine per
// attempt.
func TestStreamableCloseReleasesReadLoop(t *testing.T) {
	tr := newStreamableTransport("test", "http://127.0.0.1:1/mcp", nil)
	if err := tr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		tr.ReadLoop(func([]byte) {})
		close(stopped)
	}()

	tr.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLoop did not return after Close")
	}

	// Close must stay safe to call twice (client.Close plus reconnection).
	tr.Close()
}
