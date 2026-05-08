package approval

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNotifyOnSubmit(t *testing.T) {
	var mu sync.Mutex
	var received []notifyPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notifyPayload
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewStore(5 * time.Second)
	s.Notifier = NewNotifier(srv.URL)

	s.Submit("claude", "write_file", "rule-1", map[string]any{"path": "/tmp/x"}, "")

	// Wait for async POST
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("got %d notifications, want 1", len(received))
	}
	if received[0].Event != "pending" {
		t.Errorf("event = %q, want pending", received[0].Event)
	}
	if received[0].Tool != "write_file" {
		t.Errorf("tool = %q, want write_file", received[0].Tool)
	}
}

func TestCallbackOnResolve_SSRFBlocked(t *testing.T) {
	var mu sync.Mutex
	var received []notifyPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notifyPayload
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewStore(5 * time.Second)
	s.Notifier = NewNotifier("")

	// Callback on localhost → SSRF protection rejects it
	pa := s.Submit("claude", "gmail.send", "rule-1", nil, srv.URL)
	s.Approve(pa.ID, "admin")
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Fatalf("got %d callbacks, want 0 (SSRF protection blocks localhost)", len(received))
	}
}

func TestNotifyAndCallback(t *testing.T) {
	var mu sync.Mutex
	var received []notifyPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notifyPayload
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewStore(5 * time.Second)
	s.Notifier = NewNotifier(srv.URL)

	// Agent provides callback on localhost → SSRF-blocked, only notifyURL fires
	pa := s.Submit("claude", "write_file", "rule-1", nil, srv.URL)
	time.Sleep(50 * time.Millisecond)

	s.Deny(pa.ID, "security")
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// Only 1 event: the operator notifyURL fires (pending), but the
	// agent callbackURL on localhost is blocked by SSRF protection.
	if len(received) != 1 {
		t.Fatalf("got %d events, want 1 (pending only — callback SSRF-blocked)", len(received))
	}
	if received[0].Event != "pending" {
		t.Errorf("first event = %q, want pending", received[0].Event)
	}
}

func TestNoNotifierNoPanic(t *testing.T) {
	s := NewStore(5 * time.Second)
	// Notifier is nil — should not panic
	pa := s.Submit("claude", "tool", "r", nil, "")
	s.Approve(pa.ID, "tester")
	<-pa.Result
}

func TestCallbackOnTimeout_SSRFBlocked(t *testing.T) {
	var mu sync.Mutex
	var received []notifyPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notifyPayload
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewStore(50 * time.Millisecond)
	s.Notifier = NewNotifier("")

	// Agent-supplied callback on localhost → SSRF protection blocks it
	s.Submit("claude", "tool", "r", nil, srv.URL)

	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Fatalf("got %d callbacks, want 0 (SSRF protection should block localhost)", len(received))
	}
}

func TestSSRFCallbackValidation(t *testing.T) {
	tests := []struct {
		url  string
		safe bool
	}{
		{"http://169.254.169.254/metadata", false},
		{"http://127.0.0.1:8080/hook", false},
		{"http://localhost:9090/callback", false},
		{"http://10.0.0.1/internal", false},
		{"http://192.168.1.1/admin", false},
		{"ftp://example.com/file", false},
		{"://bad", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isSafeCallbackURL(tt.url); got != tt.safe {
			t.Errorf("isSafeCallbackURL(%q) = %v, want %v", tt.url, got, tt.safe)
		}
	}
}
