#!/usr/bin/env python3
"""Generate a test JWT + JWKS for local development.

Usage:
    # 1. Start the JWKS server (background)
    python generate-token.py serve &

    # 2. Start mesh7 with jwt-auth config (point jwks_url to localhost)
    mesh7 serve --config config.local.yaml

    # 3. Use the token
    TOKEN=$(python generate-token.py token --sub my-agent)
    curl -H "Authorization: Bearer $TOKEN" http://localhost:9090/health
"""

import argparse
import json
import sys
import time
from base64 import urlsafe_b64encode
from http.server import HTTPServer, BaseHTTPRequestHandler

try:
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.hazmat.primitives import serialization
    import jwt as pyjwt
except ImportError:
    print("pip install cryptography PyJWT", file=sys.stderr)
    sys.exit(1)


def generate_keypair():
    private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    return private_key


def private_to_jwk(private_key, kid="dev-key"):
    pub = private_key.public_key()
    pub_numbers = pub.public_numbers()
    n = pub_numbers.n.to_bytes((pub_numbers.n.bit_length() + 7) // 8, "big")
    e = pub_numbers.e.to_bytes((pub_numbers.e.bit_length() + 7) // 8, "big")
    return {
        "kty": "RSA",
        "kid": kid,
        "use": "sig",
        "alg": "RS256",
        "n": urlsafe_b64encode(n).rstrip(b"=").decode(),
        "e": urlsafe_b64encode(e).rstrip(b"=").decode(),
    }


KEY = generate_keypair()
KID = "dev-key"


def cmd_token(args):
    payload = {
        "sub": args.sub,
        "iss": args.issuer,
        "aud": args.audience,
        "exp": int(time.time()) + args.ttl,
        "iat": int(time.time()),
    }
    if args.claim and args.claim_value:
        payload[args.claim] = args.claim_value

    pem = KEY.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    token = pyjwt.encode(payload, pem, algorithm="RS256", headers={"kid": KID})
    print(token)


def cmd_jwks(_args):
    jwks = {"keys": [private_to_jwk(KEY, KID)]}
    print(json.dumps(jwks, indent=2))


def cmd_serve(args):
    jwks = json.dumps({"keys": [private_to_jwk(KEY, KID)]})

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(jwks.encode())

        def log_message(self, fmt, *a):
            pass

    port = args.port
    print(f"JWKS server on http://localhost:{port}/.well-known/jwks.json", file=sys.stderr)
    HTTPServer(("", port), Handler).serve_forever()


def main():
    p = argparse.ArgumentParser(description="JWT dev helper for mesh7")
    sub = p.add_subparsers(dest="cmd")

    t = sub.add_parser("token", help="Generate a signed JWT")
    t.add_argument("--sub", default="dev-agent")
    t.add_argument("--issuer", default="http://localhost:8888")
    t.add_argument("--audience", default="mesh7")
    t.add_argument("--ttl", type=int, default=3600)
    t.add_argument("--claim", help="Extra claim name")
    t.add_argument("--claim-value", help="Extra claim value")

    sub.add_parser("jwks", help="Print JWKS JSON")

    s = sub.add_parser("serve", help="Run a local JWKS HTTP server")
    s.add_argument("--port", type=int, default=8888)

    args = p.parse_args()
    if args.cmd == "token":
        cmd_token(args)
    elif args.cmd == "jwks":
        cmd_jwks(args)
    elif args.cmd == "serve":
        cmd_serve(args)
    else:
        p.print_help()


if __name__ == "__main__":
    main()
