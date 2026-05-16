# hermes

Deployment of Nous Research's [Hermes Agent](https://hermes-agent.nousresearch.com/) onto AKS.

Hermes is a long-lived, self-improving AI agent — persistent SQLite memory, automated skill creation (procedural memory via the `agentskills.io` open standard), Honcho-based user modeling, and a messaging gateway in front of 20+ platforms (Telegram/Slack/Discord/WhatsApp/Signal/email/CLI/…). BYO LLM, OpenAI-compatible endpoint.

This repo owns the deployment, not the agent. Upstream is [`NousResearch/hermes-agent`](https://github.com/NousResearch/hermes-agent).

## Layout

- `k8s/` — Helm chart deployed by ArgoCD. `Application` lives in `nelsong6/infra-bootstrap` at `k8s/apps/hermes.yaml`.
- `tofu/` — per-app Azure infra (workload identity, role grants). Reads `infra-bootstrap` remote state for shared values.
- `Dockerfile` — image build for `romainecr.azurecr.io/hermes` (currently a stub; see README).

## Deployment shape

- **Single-replica StatefulSet** with a PVC at `/data`. Hermes' SQLite (WAL mode) + skills directory live there; that file is the moat — without persistence, the self-improvement loop is amnesiac and Hermes is worthless. Do not change to a Deployment with emptyDir.
- **Public at `hermes.romaine.life`** via XListenerSet + cert-manager + HTTPRoute. Pattern mirrors `glimmung`.
- **Per-app workload identity** (`hermes-identity` in `tofu/identity.tf`) — narrow scope: KV Secrets User on the shared vault for the ExternalSecret fetch path, nothing else broad. Add specific role grants here as integrations are added.
- **Secrets via ExternalSecrets**: LLM API key, platform bot tokens (Telegram, Slack, …) flow from Key Vault. Keys are set manually in KV, same as glimmung's GitHub App creds. See `k8s/values.yaml::externalSecret.keys`.

## Integration with tank-operator

Hermes is a **neighbor** to tank-operator, not embedded in it. Tank session pods stay ephemeral with `emptyDir`; they reach Hermes over the network (either Tank pods become an additional Hermes platform/inbox, or Hermes exposes itself via an `mcp-hermes` MCP server that Tank pods mount). The design constraint that drives this: Hermes' value is in the persistent state, which can't live in an ephemeral pod. Don't try to make Hermes "fit inside" Tank.

## Things that don't work yet

- No real image — Dockerfile is a stub. CI workflow to build and push to `romainecr.azurecr.io/hermes` needs to be added.
- `values.yaml` env vars for Hermes' data-dir / SQLite path are placeholders; need to be confirmed against upstream config interface.
- No bot tokens loaded yet — set in KV manually before turning on a given platform.
- One Hermes-specific marketed feature (`/browser connect`, attach to the user's local browser via CDP) only works from a CLI on the user's machine; it does NOT work through any gateway (web UI, Telegram, etc.). Server-side headless browser automation works fine.

## Container build verification

Agent pods don't have Docker. Don't report missing local Docker as a blocker. Use the repo's PR CI workflow `.github/workflows/docker-build-check.yaml` (TODO — not yet added) for image-build feedback before a PR is ready; trigger manually with `git_ref` if needed. Release/deploy workflow is the only image-publishing path.
