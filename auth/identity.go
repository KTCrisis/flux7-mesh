package auth

import (
	"errors"
	"strings"
)

// ErrIdentityRequired is returned when, with a JWT validator configured, the
// caller presents no usable credential (or only a plaintext "agent:" claim
// while legacy mode is off).
var ErrIdentityRequired = errors.New("authentication required: present a valid JWT")

// ResolveAgentID derives the agent identity from an Authorization header value.
// It is the single source of truth for both HTTP entry points (the REST proxy
// and the MCP Streamable HTTP transport) so the two cannot drift apart.
//
// Two postures, decided by whether a validator is configured:
//
//   - validator == nil (no JWT): local posture. "Bearer agent:<id>" yields <id>;
//     any other non-empty bearer is taken verbatim as the agent id. This is the
//     unauthenticated, localhost-only posture — identity is self-declared.
//
//   - validator != nil (JWT configured): strict posture. Identity must come from
//     a validated JWT. The plaintext "agent:<id>" form is REJECTED so it cannot
//     be used to spoof past cryptographic validation — unless allowLegacy is
//     explicitly set, which re-enables the bypass for migration scenarios.
//
// An empty header resolves to "anonymous" in both postures.
func ResolveAgentID(authHeader string, v *Validator, allowLegacy bool) (string, error) {
	raw := strings.TrimPrefix(authHeader, "Bearer ")
	if raw == "" {
		return "anonymous", nil
	}
	hasAgentPrefix := strings.HasPrefix(raw, "agent:")

	if v != nil {
		// Strict: a configured JWT validator is the source of truth.
		if hasAgentPrefix {
			if allowLegacy {
				return strings.TrimPrefix(raw, "agent:"), nil
			}
			return "", ErrIdentityRequired
		}
		if strings.Count(raw, ".") == 2 {
			return v.ValidateToken(raw)
		}
		return "", ErrIdentityRequired
	}

	// Local posture: no JWT configured, identity is self-declared.
	if hasAgentPrefix {
		return strings.TrimPrefix(raw, "agent:"), nil
	}
	return raw, nil
}
