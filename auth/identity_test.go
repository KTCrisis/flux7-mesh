package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestResolveAgentID_NoJWT(t *testing.T) {
	// Local posture: no validator. agent: prefix and raw bearer both accepted.
	cases := []struct{ header, want string }{
		{"", "anonymous"},
		{"Bearer agent:alice", "alice"},
		{"Bearer some-token", "some-token"},
	}
	for _, c := range cases {
		got, err := ResolveAgentID(c.header, nil, false)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", c.header, err)
		}
		if got != c.want {
			t.Errorf("%q → %q, want %q", c.header, got, c.want)
		}
	}
}

func TestResolveAgentID_JWTStrictRejectsAgentPrefix(t *testing.T) {
	priv, v := testKey(t)
	defer v.Close()

	// The spoofing attempt: claim an identity with no proof while JWT is on.
	_, err := ResolveAgentID("Bearer agent:admin", v, false)
	if err == nil {
		t.Fatal("agent: prefix must be rejected when JWT is configured (spoofing)")
	}

	// A bare non-JWT bearer is also rejected — identity must be a valid JWT.
	if _, err := ResolveAgentID("Bearer random-token", v, false); err == nil {
		t.Fatal("non-JWT bearer must be rejected in strict mode")
	}

	// A valid JWT resolves to its subject.
	tok := signToken(t, priv, jwt.MapClaims{"sub": "ci-bot", "exp": time.Now().Add(time.Hour).Unix()})
	got, err := ResolveAgentID("Bearer "+tok, v, false)
	if err != nil {
		t.Fatalf("valid JWT rejected: %v", err)
	}
	if got != "ci-bot" {
		t.Errorf("got %q, want ci-bot", got)
	}

	// Empty header is still anonymous, even in strict mode.
	if got, err := ResolveAgentID("", v, false); err != nil || got != "anonymous" {
		t.Errorf("empty header → (%q, %v), want (anonymous, nil)", got, err)
	}
}

func TestResolveAgentID_AllowLegacyReopensBypass(t *testing.T) {
	_, v := testKey(t)
	defer v.Close()

	// Escape hatch: allowLegacy=true re-enables agent: even with JWT on.
	got, err := ResolveAgentID("Bearer agent:admin", v, true)
	if err != nil {
		t.Fatalf("allowLegacy should accept agent: prefix, got %v", err)
	}
	if got != "admin" {
		t.Errorf("got %q, want admin", got)
	}
}
