# hermes

Deployment of Nous Research's [Hermes Agent](https://hermes-agent.nousresearch.com/) onto AKS.

Hermes is a long-lived, self-improving AI agent — persistent SQLite memory, automated skill creation (procedural memory via the `agentskills.io` open standard), Honcho-based user modeling, and a messaging gateway in front of 20+ platforms (Telegram/Slack/Discord/WhatsApp/Signal/email/CLI/…). BYO LLM, OpenAI-compatible endpoint.

This repo owns the deployment, not the agent. Upstream is [`NousResearch/hermes-agent`](https://github.com/NousResearch/hermes-agent).

## Layout

- `k8s/` — Helm chart deployed by ArgoCD. `Application` lives in `nelsong6/infra-bootstrap` at `k8s/apps/hermes.yaml`.
- `tofu/` — per-app Azure infra (workload identity, role grants). Reads `infra-bootstrap` remote state for shared values.
- `Dockerfile` — multi-stage build for `romainecr.azurecr.io/hermes`. Pinned to upstream `v0.14.0` via `pip install "hermes-agent[all] @ git+..."`. Entrypoint is `hermes gateway`.
- `.github/workflows/` — `tofu.yaml` (plan-on-PR / apply-on-push, via the shared `pipeline-templates` template), `build.yaml` (image build + push to ACR, auto-commits the new tag into `k8s/values.yaml`), `docker-build-check.yaml` (PR-time build verification).

## Deployment shape

- **Single-replica StatefulSet** with a PVC at `/data`. Hermes' SQLite (WAL mode) + skills directory live there; that file is the moat — without persistence, the self-improvement loop is amnesiac and Hermes is worthless. Do not change to a Deployment with emptyDir.
- **Public at `hermes.romaine.life`** via XListenerSet + cert-manager + HTTPRoute. Pattern mirrors `glimmung`.
- **Per-app workload identity** (`hermes-identity` in `tofu/identity.tf`) — narrow scope: KV Secrets User on the shared vault for the ExternalSecret fetch path, nothing else broad. Add specific role grants here as integrations are added.
- **Secrets via ExternalSecrets**: LLM API key, platform bot tokens (Telegram, Slack, …) flow from Key Vault. Keys are set manually in KV, same as glimmung's GitHub App creds. See `k8s/values.yaml::externalSecret.keys`.

## Integration with tank-operator

Hermes is a **neighbor** to tank-operator, not embedded in it. Tank session pods stay ephemeral with `emptyDir`; they reach Hermes over the network (either Tank pods become an additional Hermes platform/inbox, or Hermes exposes itself via an `mcp-hermes` MCP server that Tank pods mount). The design constraint that drives this: Hermes' value is in the persistent state, which can't live in an ephemeral pod. Don't try to make Hermes "fit inside" Tank.

## Things that don't work yet

- No LLM API key set in Key Vault. The ExternalSecret expects `hermes-llm-api-key` (env-mapped to `OPENROUTER_API_KEY` by default; edit `values.yaml::externalSecret.keys` if Hermes is pointed at another provider). Without it, the gateway will start but fail real requests.
- No platform bot tokens loaded. Each platform (Telegram, Slack, …) needs its KV secret + an entry in `externalSecret.keys` before that platform comes online.
- The `[browser]` extra installs without the chromium runtime; if/when in-pod browser automation is exercised, install chromium in the Dockerfile runtime stage.
- One Hermes-specific marketed feature (`/browser connect`, attach to the user's local browser via CDP) only works from a CLI on the user's machine; it does NOT work through any gateway (web UI, Telegram, etc.). Server-side headless browser automation works fine.

## Container build verification

Agent pods don't have Docker. Don't report missing local Docker as a blocker. Use `.github/workflows/docker-build-check.yaml` for image-build feedback before a PR is ready; trigger manually with `git_ref` if needed. `build.yaml` on push-to-main is the only image-publishing path.
