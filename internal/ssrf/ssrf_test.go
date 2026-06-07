package ssrf

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckURL(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"ftp://example.com",
		"http://localhost/x",
		"://nonsense",
	}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) = nil, want error", u)
		}
	}
	good := []string{"http://example.com/spec.json", "https://api.example.com/v1"}
	for _, u := range good {
		if err := CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestIsBlocked(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0"}
	for _, s := range blocked {
		if !isBlocked(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	// A public address must pass.
	if isBlocked(net.ParseIP("93.184.216.34")) {
		t.Error("public IP should not be blocked")
	}
}

func TestClientRefusesLoopbackTarget(t *testing.T) {
	// A real loopback server: the guarded client must refuse to dial it, even
	// though it is reachable, because the resolved IP is loopback.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := Client(2 * time.Second)
	_, err := c.Get(srv.URL) // srv.URL is http://127.0.0.1:port
	if err == nil {
		t.Fatal("guarded client should refuse a loopback target")
	}
	if !strings.Contains(err.Error(), "ssrf") {
		t.Errorf("expected ssrf block error, got %v", err)
	}
}
