# hermes

Deployment of [Hermes Agent](https://hermes-agent.nousresearch.com/) (Nous Research) onto the shared AKS cluster.

Hermes is a long-lived, self-improving AI agent: persistent SQLite memory (FTS5 cross-session recall + LLM summarization), automated skill creation, Honcho user modeling, and a messaging gateway that fronts 20+ platforms (Telegram, Slack, Discord, WhatsApp, Signal, email, CLI, …).

## Layout

```
.
├── Dockerfile          # Image build for romainecr.azurecr.io/hermes (TODO)
├── k8s/                # Helm chart deployed by ArgoCD (Application in infra-bootstrap)
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/      # namespace, SA, statefulset, service, httproute, cert, externalsecret
└── tofu/               # Per-app Azure: workload identity, role grants, KV secret stubs
```

## Deployment model

- **Single-replica StatefulSet** with a PVC at `/data` for the SQLite + skills directory. Sized for ~10 concurrent sessions; SQLite WAL mode handles that comfortably.
- **Public ingress** at `hermes.romaine.life` — XListenerSet + Certificate + HTTPRoute, same pattern as `glimmung`.
- **Workload identity** via the per-app AAD app provisioned by `infra-bootstrap`'s `module.app["hermes"]`, plus a dedicated `hermes-identity` managed identity from `tofu/identity.tf` for KV reads.
- **Secrets** (LLM API key, platform bot tokens) flow via ExternalSecrets from Key Vault, populated manually — same pattern as glimmung's GitHub App credentials.

## Integration intent

Hermes is intended to sit as a **neighbor** to tank-operator, not embed inside it. Tank session pods become additional clients of the same Hermes instance (alongside Telegram/Slack/etc.) — they don't run their own Hermes. See conversation history in #1 / `romaine-life/infra-bootstrap#122` for the design discussion.

## Status

**Scaffolded, not yet deployed.** Open TODOs before the StatefulSet will boot:

- Build a real container image and push to `romainecr.azurecr.io/hermes` (Dockerfile is a stub).
- The chart currently uses `anthropic-api-key` from the app-owned `ng6-hermes` Key Vault, mounted as `ANTHROPIC_API_KEY`, and renders `model.provider=anthropic` into Hermes' `/data/config.yaml`. To move back to OpenRouter, replace the `hermes-llm-api-key` placeholder with a real key and update `externalSecret.keys` plus `config.model`.
- Confirm any additional upstream env vars before enabling platform gateways; the current chart is validated for the API-server model path.

## Related repos

- `romaine-life/infra-bootstrap` — root infra; owns the AAD app, OIDC fed creds, ArgoCD Application
- `romaine-life/tank-operator` — sister AI agent platform (ephemeral session pods, contrast: Hermes is long-lived)
- Upstream: [`NousResearch/hermes-agent`](https://github.com/NousResearch/hermes-agent)
