# JWT Auth Example

mesh7 validates JWT tokens from an external identity provider.
Agents are identified by a claim in the JWT (default: `sub`).

## Config

```yaml
auth:
  jwt:
    jwks_url: https://auth.example.com/.well-known/jwks.json
    issuer: https://auth.example.com
    audience: mesh7
    agent_claim: sub
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `jwks_url` | Yes | - | JWKS endpoint for public keys |
| `issuer` | No | - | Validate `iss` claim |
| `audience` | No | - | Validate `aud` claim |
| `agent_claim` | No | `sub` | Which claim = agent ID |

No `auth.jwt` block = no validation (backward compatible with `Bearer agent:<name>`).

## Local dev

```bash
# Install deps for the token generator
pip install cryptography PyJWT

# Terminal 1: JWKS server
python generate-token.py serve

# Terminal 2: mesh7
mesh7 serve --config config.local.yaml

# Terminal 3: test
TOKEN=$(python generate-token.py token --sub my-agent)
curl -H "Authorization: Bearer $TOKEN" http://localhost:9090/health
curl -H "Authorization: Bearer $TOKEN" http://localhost:9090/tools

# Invalid token → 401
curl -H "Authorization: Bearer bad.token.here" http://localhost:9090/tools

# Legacy agent: prefix still works (no JWT validation)
curl -H "Authorization: Bearer agent:dev" http://localhost:9090/tools
```

## Production IdPs

### Cloudflare Access

```yaml
auth:
  jwt:
    jwks_url: https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
    issuer: https://<team>.cloudflareaccess.com
    audience: <application-aud-tag>
    agent_claim: email
```

### Auth0

```yaml
auth:
  jwt:
    jwks_url: https://<tenant>.auth0.com/.well-known/jwks.json
    issuer: https://<tenant>.auth0.com/
    audience: https://mesh.example.com
```

### Keycloak

```yaml
auth:
  jwt:
    jwks_url: https://<host>/realms/<realm>/protocol/openid-connect/certs
    issuer: https://<host>/realms/<realm>
    audience: mesh7
```
