# auth-proxy

Tiny Go reverse proxy that fronts the `hermes dashboard` with cookie-based auth against `auth.romaine.life`. Lifted from `romaine-life/glimmung`'s `internal/auth/cookie_delegate.go` — same shape, repackaged as a standalone HTTP service so it can sit in front of an upstream app we don't control (the FastAPI dashboard from `NousResearch/hermes-agent`).

## Behavior

For every inbound request:

1. `GET /healthz` and `/readyz` bypass auth (kubelet probes).
2. Extract the `Cookie` header. Empty → 401 (or 302 to sign-in for browser GETs).
3. Forward the cookie to `https://auth.romaine.life/api/auth/get-session`. Cache the result for 60s per cookie value.
4. From the session response:
   - `role == "admin"` → **allow**.
   - `role == "user"` AND `apps[REQUIRED_APP_KEY].access === true` → **allow**.
   - anything else → **403**.
5. On allow, reverse-proxy to `UPSTREAM_URL` with identity headers attached:
   - `X-Forwarded-User: <email>`
   - `X-Forwarded-User-Sub: <id>`
   - `X-Forwarded-User-Role: <role>`
6. On 401 (no/invalid cookie) AND the caller looks like a browser GET (Accept: text/html), **302** to `auth.romaine.life`'s Microsoft sign-in, with the original URL as `callbackURL`. Non-browser callers get a plain 401 + `WWW-Authenticate`.

The auth-side `apps` column is a free-form JSON string ([Better Auth additionalFields](https://www.better-auth.com)). Admins set `{"hermes":{"access":true}}` in the Tyrell Console at `auth.romaine.life/admin`. The proxy parses arbitrary keys — no schema lock.

## Config (env vars)

| Var | Required | Default | Notes |
| --- | --- | --- | --- |
| `UPSTREAM_URL` | yes | — | Absolute URL. e.g. `http://127.0.0.1:9119` |
| `PUBLIC_HOSTNAME` | yes | — | Externally-visible hostname for building the post-sign-in callback. e.g. `hermes.romaine.life` |
| `REQUIRED_APP_KEY` | no | empty (admin-only) | Key in the auth-side user's `apps` JSON to require with `.access=true`. Empty = no per-app gate; only admins pass. |
| `AUTH_SESSION_URL` | no | `https://auth.romaine.life/api/auth/get-session` | |
| `AUTH_SIGNIN_URL` | no | `https://auth.romaine.life/api/auth/sign-in/social/microsoft` | |
| `LISTEN_ADDR` | no | `:8080` | |

## Caching

A 60s in-process map of `cookie value → session result`. Errors are cached too — a 401 doesn't re-pound auth.romaine.life on every request inside the window. Max 200 entries; lazy purge on insert when full.

## Why this exists and not glimmung's middleware

Glimmung's `cookie_delegate.go` is middleware inside the glimmung Go binary. Hermes' upstream is Python FastAPI we don't want to fork, so the same logic moves out into a sidecar that's language-agnostic. The cookie-delegate code path is functionally identical; the proxy wrapping is the only addition.

## Future additions (not done)

- **Service-to-service via `role=service` JWTs.** `auth.romaine.life`'s recent SA token exchange (`/api/auth/exchange/k8s`) mints `role=service` JWTs for in-cluster pods on behalf of their human owner (`actor_email` claim). When a Tank session pod needs to talk to Hermes, that's the right path — but it presents `Authorization: Bearer <jwt>`, not a cookie. The proxy currently doesn't handle bearer tokens. Add a JWKS-verify branch when there's a real consumer.

## Testing

`go test ./...` from `auth-proxy/`. Covers the cookie → session → role/apps gate, the cache, the redirect-vs-401 branching, the cookie strip on the way upstream.
