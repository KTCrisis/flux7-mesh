# In the local posture, a JWT is refused, not taken as a name

- **Problem**: with no `auth.jwt`, any bearer other than `agent:<id>` became the agent id verbatim; assay7's forged-token probe was recorded as an agent named `eyJhbGciOi...`, in the traces and on the console overview.
- **Decision**: in the local posture a bearer shaped like a JWT (three parts, header starting `eyJ`) is refused with `ErrJWTWithoutValidator` (401, traced as `auth:invalid_credential` under `anonymous`); `agent:<id>` and other plain bearers keep their meaning.
- **Why**: nothing can verify a JWT without a validator, and a credential must never be recorded as an identity; a client sending JWTs to a mesh without `auth.jwt` is a configuration error worth an explicit refusal.
- **Where**: `auth/identity.go` (`looksLikeJWT`, `ResolveIdentity`), test in `auth/jwt_test.go`. The traces already written keep the old names: the journal is append-only.
