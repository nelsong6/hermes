# hermes

Deployment of Nous Research's [Hermes Agent](https://hermes-agent.nousresearch.com/) onto AKS.

Hermes is a long-lived, self-improving AI agent — persistent SQLite memory, automated skill creation (procedural memory via the `agentskills.io` open standard), Honcho-based user modeling, and a messaging gateway in front of 20+ platforms (Telegram/Slack/Discord/WhatsApp/Signal/email/CLI/…). BYO LLM, OpenAI-compatible endpoint.

This repo owns the deployment, not the agent. Upstream is [`NousResearch/hermes-agent`](https://github.com/NousResearch/hermes-agent).

## Layout

- `k8s/` — Helm chart deployed by ArgoCD. `Application` lives in `romaine-life/infra-bootstrap` at `k8s/apps/hermes.yaml`.
- `tofu/` — per-app Azure infra (workload identity, role grants). Reads `infra-bootstrap` remote state for shared values.
- `Dockerfile` — multi-stage build for `romainecr.azurecr.io/hermes`. Pinned to upstream `v2026.5.16` (date-tagged release scheme) via `pip install "hermes-agent[all] @ git+..."`. Entrypoint is `hermes gateway`.
- `auth-proxy/` — Go reverse proxy built into a separate image `romainecr.azurecr.io/auth-proxy`. Tiny (~250 LOC + tests). See `auth-proxy/README.md` for env-var config.
- `.github/workflows/` — `tofu.yaml` (plan-on-PR / apply-on-push, via the shared `pipeline-templates` template), `build.yaml` (builds **both** images, auto-commits both new tags into `k8s/values.yaml` in a single commit so ArgoCD rolls them together), `docker-build-check.yaml` (PR-time matrix build verification for both images).

## Deployment shape

- **Single-replica StatefulSet** with a PVC at `/data`. Hermes' SQLite (WAL mode) + skills directory live there; that file is the moat — without persistence, the self-improvement loop is amnesiac and Hermes is worthless. Do not change to a Deployment with emptyDir.
- **Single hermes container** runs both the gateway (foreground) and the dashboard (backgrounded by our `entrypoint.sh`, matching upstream's `docker/entrypoint.sh`). Upstream docs explicitly warn against running these as separate containers — the dashboard's gateway-liveness detection requires a shared PID namespace. The dashboard binds `127.0.0.1:9119` and is fronted by the auth-proxy sidecar. `HERMES_DASHBOARD_TUI=1` is what makes the in-dashboard Chat tab appear (`web/src/lib/dashboard-flags.ts` gates the route on `window.__HERMES_DASHBOARD_EMBEDDED_CHAT__`).
- **Public at `hermes.romaine.life`** via XListenerSet + cert-manager + HTTPRoute. Pattern mirrors `glimmung`.
- **Auth gate: `auth-proxy` sidecar** (Go reverse proxy at `auth-proxy/`, lifted from glimmung's `internal/auth/cookie_delegate.go`). Fronts the dashboard at port 8080 (the Service's targetPort); reverse-proxies authenticated traffic to the dashboard on `127.0.0.1:9119` inside the pod. Per-request flow: forward the inbound `.romaine.life` session cookie to `auth.romaine.life/api/auth/get-session`, cache 60s, allow iff `role=admin` OR `apps.hermes.access=true`. Unauthed browser GETs 302 to Entra via `auth.romaine.life/api/auth/sign-in/social/microsoft?callbackURL=...`. Non-browser callers (Accept: not html) get a 401 + WWW-Authenticate rather than a redirect.
- **Granting access to a new user**: admin opens `auth.romaine.life/admin`, finds the user, sets `apps` JSON to include `{"hermes":{"access":true}}` (admins bypass automatically). No code changes needed — the `apps` column is a free-form JSON textarea and the proxy parses arbitrary keys. Service-to-service auth via the K8s SA exchange (`role=service`) is a future addition; the proxy currently handles cookies only.
- **Per-app workload identity** (`hermes-identity` in `tofu/identity.tf`) — narrow scope for app-specific Azure access. ExternalSecrets reads from the Hermes-owned `ng6-hermes` vault. Add specific role grants here as integrations are added.
- **Secrets via ExternalSecrets**: LLM API key, platform bot tokens (Telegram, Slack, …) flow from Key Vault. Keys are set manually in KV, same as glimmung's GitHub App creds. See `k8s/values.yaml::externalSecret.keys`.

## Integration with tank-operator

Hermes is a **neighbor** to tank-operator, not embedded in it. Tank session pods stay ephemeral with `emptyDir`; they reach Hermes over the network (either Tank pods become an additional Hermes platform/inbox, or Hermes exposes itself via an `mcp-hermes` MCP server that Tank pods mount). The design constraint that drives this: Hermes' value is in the persistent state, which can't live in an ephemeral pod. Don't try to make Hermes "fit inside" Tank.

## Things that don't work yet

- Hermes uses `anthropic-api-key` from the app-owned `ng6-hermes` Key Vault by default, mapped to `ANTHROPIC_API_KEY`, with `model.provider=anthropic` in the chart-rendered config. The older `hermes-llm-api-key` OpenRouter placeholder is not used until it is replaced with a real key and the chart is pointed back at OpenRouter.
- No platform bot tokens loaded. Each platform (Telegram, Slack, …) needs its KV secret + an entry in `externalSecret.keys` before that platform comes online.
- The `[browser]` extra installs without the chromium runtime; if/when in-pod browser automation is exercised, install chromium in the Dockerfile runtime stage.
- One Hermes-specific marketed feature (`/browser connect`, attach to the user's local browser via CDP) only works from a CLI on the user's machine; it does NOT work through any gateway (web UI, Telegram, etc.). Server-side headless browser automation works fine.

## Container build verification

Agent pods don't have Docker. Don't report missing local Docker as a blocker. Use `.github/workflows/docker-build-check.yaml` for image-build feedback before a PR is ready; trigger manually with `git_ref` if needed. `build.yaml` on push-to-main is the only image-publishing path.
