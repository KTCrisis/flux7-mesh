package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKey(t *testing.T) (*rsa.PrivateKey, *Validator) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidatorWithKeys(map[string]any{
		"test-key": &priv.PublicKey,
	})
	return priv, v
}

func signToken(t *testing.T, priv *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-key"
	s, err := token.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidateTokenValid(t *testing.T) {
	priv, v := testKey(t)
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"sub": "my-agent",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	agent, err := v.ValidateToken(tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "my-agent" {
		t.Errorf("agent = %q, want my-agent", agent)
	}
}

func TestValidateTokenExpired(t *testing.T) {
	priv, v := testKey(t)
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"sub": "my-agent",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestValidateTokenBadSignature(t *testing.T) {
	_, v := testKey(t)
	defer v.Close()

	// Sign with a different key
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signToken(t, otherKey, jwt.MapClaims{
		"sub": "evil",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for bad signature")
	}
}

func TestValidateTokenBadIssuer(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := NewValidatorWithKeys(
		map[string]any{"test-key": &priv.PublicKey},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("https://expected.example.com"),
	)
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"sub": "agent",
		"iss": "https://wrong.example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestValidateTokenBadAudience(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := NewValidatorWithKeys(
		map[string]any{"test-key": &priv.PublicKey},
		jwt.WithExpirationRequired(),
		jwt.WithAudience("mesh7"),
	)
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"sub": "agent",
		"aud": "wrong-audience",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestValidateTokenMissingClaim(t *testing.T) {
	priv, v := testKey(t)
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
		// no "sub" claim
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for missing sub claim")
	}
}

func TestValidateTokenMissingKid(t *testing.T) {
	priv, v := testKey(t)
	defer v.Close()

	// Create token without kid
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "agent",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	// Don't set kid
	delete(token.Header, "kid")
	tok, _ := token.SignedString(priv)

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for missing kid")
	}
}

func TestValidateTokenCustomClaim(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := NewValidatorWithKeys(
		map[string]any{"test-key": &priv.PublicKey},
		jwt.WithExpirationRequired(),
	)
	v.agentClaim = "agent_id"
	defer v.Close()

	tok := signToken(t, priv, jwt.MapClaims{
		"agent_id": "custom-bot",
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	agent, err := v.ValidateToken(tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "custom-bot" {
		t.Errorf("agent = %q, want custom-bot", agent)
	}
}

func TestValidateTokenUnknownKid(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := NewValidatorWithKeys(map[string]any{
		"other-key": &priv.PublicKey,
	})
	defer v.Close()

	// Token signed with kid "test-key" but validator only knows "other-key"
	tok := signToken(t, priv, jwt.MapClaims{
		"sub": "agent",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.ValidateToken(tok)
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
}
