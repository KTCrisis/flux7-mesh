package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/KTCrisis/flux7-mesh/config"
)

// Validator validates JWT tokens against a JWKS endpoint.
// Thread-safe: keys are cached and refreshed periodically.
type Validator struct {
	parser     *jwt.Parser
	agentClaim string
	jwksURL    string

	mu   sync.RWMutex
	keys map[string]any // kid → *rsa.PublicKey or *ecdsa.PublicKey

	stop chan struct{}
}

// NewValidator creates a JWT validator from config.
// Fetches JWKS immediately and starts a background refresh goroutine.
func NewValidator(cfg *config.JWTConfig) (*Validator, error) {
	opts := []jwt.ParserOption{
		jwt.WithExpirationRequired(),
	}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}

	claim := cfg.AgentClaim
	if claim == "" {
		claim = "sub"
	}

	v := &Validator{
		parser:     jwt.NewParser(opts...),
		agentClaim: claim,
		jwksURL:    cfg.JWKSURL,
		keys:       make(map[string]any),
		stop:       make(chan struct{}),
	}

	if err := v.fetchKeys(); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch: %w", err)
	}

	go v.refreshLoop()
	return v, nil
}

// NewValidatorWithKeys creates a validator with pre-loaded keys (for testing).
func NewValidatorWithKeys(keys map[string]any, opts ...jwt.ParserOption) *Validator {
	if len(opts) == 0 {
		opts = []jwt.ParserOption{jwt.WithExpirationRequired()}
	}
	return &Validator{
		parser:     jwt.NewParser(opts...),
		agentClaim: "sub",
		keys:       keys,
		stop:       make(chan struct{}),
	}
}

// Close stops the background JWKS refresh.
func (v *Validator) Close() {
	close(v.stop)
}

// ValidateToken parses and validates a JWT, returning the agent ID from the configured claim.
func (v *Validator) ValidateToken(tokenStr string) (string, error) {
	token, err := v.parser.Parse(tokenStr, v.keyFunc)
	if err != nil {
		return "", fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("unexpected claims type")
	}

	agent, ok := claims[v.agentClaim]
	if !ok {
		return "", fmt.Errorf("missing claim %q", v.agentClaim)
	}

	agentStr, ok := agent.(string)
	if !ok {
		return "", fmt.Errorf("claim %q is not a string", v.agentClaim)
	}

	return agentStr, nil
}

func (v *Validator) keyFunc(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("token missing kid header")
	}

	v.mu.RLock()
	key, ok := v.keys[kid]
	v.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown key id: %s", kid)
	}
	return key, nil
}

// fetchKeys downloads and parses the JWKS from the configured URL.
func (v *Validator) fetchKeys() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(v.jwksURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []jwkKey `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	parsed := make(map[string]any, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kid == "" {
			continue
		}
		key, err := k.toPublicKey()
		if err != nil {
			slog.Warn("skipping JWKS key", "kid", k.Kid, "err", err)
			continue
		}
		parsed[k.Kid] = key
	}

	if len(parsed) == 0 {
		return fmt.Errorf("no usable keys in JWKS response")
	}

	v.mu.Lock()
	v.keys = parsed
	v.mu.Unlock()

	slog.Info("JWKS refreshed", "keys", len(parsed), "url", v.jwksURL)
	return nil
}

func (v *Validator) refreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			if err := v.fetchKeys(); err != nil {
				slog.Error("JWKS refresh failed", "err", err)
			}
		}
	}
}

// jwkKey represents a single key in a JWKS response.
type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	// RSA
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
	// ECDSA
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

func (k jwkKey) toPublicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		return k.toRSA()
	case "EC":
		return k.toECDSA()
	default:
		return nil, fmt.Errorf("unsupported key type: %s", k.Kty)
	}
}

func (k jwkKey) toRSA() (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: int(new(big.Int).SetBytes(eb).Int64()),
	}, nil
}

func (k jwkKey) toECDSA() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported curve: %s", k.Crv)
	}
	xb, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	yb, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("decode y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xb),
		Y:     new(big.Int).SetBytes(yb),
	}, nil
}
