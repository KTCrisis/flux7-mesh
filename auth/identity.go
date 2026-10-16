package auth

import (
	"errors"
	"strings"
)

// ErrIdentityRequired is returned when, with a JWT validator configured, the
// caller presents no usable credential (or only a plaintext "agent:" claim
// while legacy mode is off).
var ErrIdentityRequired = errors.New("authentication required: present a valid JWT")

// ErrJWTWithoutValidator is returned in the local posture when the bearer is
// a JWT: with no validator nothing can check it, and taking the token text
// as the agent's name would record a credential as an identity (and show it
// in every trace and dashboard).
var ErrJWTWithoutValidator = errors.New("a JWT was presented but this mesh has no JWT validator configured (auth.jwt)")

// ResolveAgentID derives the agent identity from an Authorization header value.
// It is the single source of truth for both HTTP entry points (the REST proxy
// and the MCP Streamable HTTP transport) so the two cannot drift apart.
//
// Two postures, decided by whether a validator is configured:
//
//   - validator == nil (no JWT): local posture. "Bearer agent:<id>" yields <id>;
//     a bearer shaped like a JWT is refused (nothing can check it); any other
//     non-empty bearer is taken verbatim as the agent id. This is the
//     unauthenticated, localhost-only posture — identity is self-declared.
//
//   - validator != nil (JWT configured): strict posture. Identity must come from
//     a validated JWT. The plaintext "agent:<id>" form is REJECTED so it cannot
//     be used to spoof past cryptographic validation — unless allowLegacy is
//     explicitly set, which re-enables the bypass for migration scenarios.
//
// An empty header resolves to "anonymous" in both postures.
func ResolveAgentID(authHeader string, v *Validator, allowLegacy bool) (string, error) {
	id, err := ResolveIdentity(authHeader, v, allowLegacy)
	return id.AgentID, err
}

// ResolveIdentity is ResolveAgentID with the user half kept. Only a validated
// JWT can carry a user; the self-declared "agent:<id>" form and the local
// posture never do, so UserID is empty there by construction.
func ResolveIdentity(authHeader string, v *Validator, allowLegacy bool) (Identity, error) {
	raw := strings.TrimPrefix(authHeader, "Bearer ")
	if raw == "" {
		return Identity{AgentID: "anonymous"}, nil
	}
	hasAgentPrefix := strings.HasPrefix(raw, "agent:")

	if v != nil {
		// Strict: a configured JWT validator is the source of truth.
		if hasAgentPrefix {
			if allowLegacy {
				return Identity{AgentID: strings.TrimPrefix(raw, "agent:")}, nil
			}
			return Identity{}, ErrIdentityRequired
		}
		if strings.Count(raw, ".") == 2 {
			return v.ValidateIdentity(raw)
		}
		return Identity{}, ErrIdentityRequired
	}

	// Local posture: no JWT configured, identity is self-declared.
	if hasAgentPrefix {
		return Identity{AgentID: strings.TrimPrefix(raw, "agent:")}, nil
	}
	if looksLikeJWT(raw) {
		return Identity{}, ErrJWTWithoutValidator
	}
	return Identity{AgentID: raw}, nil
}

// looksLikeJWT: three dot-separated parts, the header a base64url JSON object
// ("eyJ" is the encoding of `{"`). Shape only; nothing is verified here.
func looksLikeJWT(raw string) bool {
	return strings.Count(raw, ".") == 2 && strings.HasPrefix(raw, "eyJ")
}
