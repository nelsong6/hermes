# Hermes Agent container image.
#
# Upstream is github.com/NousResearch/hermes-agent. It ships as a Python
# package with a `hermes` console_script declared in pyproject.toml, plus
# a Vite + React SPA under `web/` that the `hermes dashboard` server
# expects pre-built into `hermes_cli/web_dist/`. A pure `pip install`
# from the git tag only installs the Python side, leaving the dashboard
# returning {"error":"Frontend not built. Run: cd web && npm run build"}
# — so we do the npm build ourselves in a Node stage, then pip-install
# from the local tree (which now carries the built web_dist/).
#
# Pinned to v2026.5.16 (upstream's latest date-tagged release as of
# 2026-05-16). Upstream ships date-tagged releases (`v<YYYY>.<M>.<D>`),
# not semver — see https://github.com/NousResearch/hermes-agent/tags.
# Bump deliberately; releases may touch Hermes' persistent SQLite schema
# only at boundaries upstream documents.
ARG HERMES_VERSION=v2026.5.16

# ─── stage 1: clone source + build frontend ────────────────────────────
FROM node:20-alpine AS web-build
ARG HERMES_VERSION
RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 --branch ${HERMES_VERSION} \
      https://github.com/NousResearch/hermes-agent.git .
WORKDIR /src/web
# Use npm ci for reproducible installs against the lockfile; fall back
# to npm install when the lockfile is missing (some upstream tags ship
# without one). The Vite build writes to ../hermes_cli/web_dist/ per
# upstream's web/README.md.
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi \
 && npm run build \
 && test -d /src/hermes_cli/web_dist || (echo "expected /src/hermes_cli/web_dist after npm build" >&2; exit 1)

# ─── stage 2: pip install from the local source tree ──────────────────
FROM python:3.11-slim AS py-build
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# git for any git+... transitive deps the package may pull in;
# build-essential for sdists that need to compile C extensions.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git build-essential \
 && rm -rf /var/lib/apt/lists/*

RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"

# Bring the source (with built web_dist) over from the web-build stage.
COPY --from=web-build /src /src

# `[all]` pulls in the optional platform connectors + the `web` extra
# (FastAPI + uvicorn for the dashboard).
RUN pip install "/src[all]"

# Belt-and-suspenders: ensure web_dist landed inside the installed
# package directory regardless of how upstream's pyproject.toml declares
# its package-data. The FastAPI dashboard resolves the SPA path
# relative to its own package, so it has to live alongside the .py
# files. Idempotent — if pip-install already copied it via
# package-data, this overwrites with identical content.
RUN python - <<'PY'
import hermes_cli, os, shutil
dst = os.path.join(os.path.dirname(hermes_cli.__file__), "web_dist")
src = "/src/hermes_cli/web_dist"
shutil.copytree(src, dst, dirs_exist_ok=True)
print(f"web_dist installed at {dst}, files:", len(os.listdir(dst)))
PY

# ─── stage 3: runtime ──────────────────────────────────────────────────
FROM python:3.11-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:${PATH}"

# tini for proper signal handling. Headless browser deps live behind
# Hermes' `[browser]` extra; install chromium here if/when that path is
# exercised in-pod.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      tini \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 hermes \
 && mkdir -p /data \
 && chown -R hermes:hermes /data

COPY --from=py-build --chown=hermes:hermes /opt/venv /opt/venv
COPY --chown=hermes:hermes entrypoint.sh /usr/local/bin/hermes-entrypoint.sh
RUN chmod +x /usr/local/bin/hermes-entrypoint.sh

USER hermes
WORKDIR /home/hermes
VOLUME ["/data"]

ENV HERMES_HOME=/data \
    HERMES_DATA_DIR=/data

# 8642 — gateway HTTP probe (used by dashboard's gateway-liveness check
# when GATEWAY_HEALTH_URL is set; not currently exposed to the cluster).
# 9119 — dashboard SPA + WebSocket (auth-proxy sidecar reaches this via
# 127.0.0.1 in the pod's network namespace).
EXPOSE 8642 9119

# The entrypoint backgrounds `hermes dashboard` when HERMES_DASHBOARD=1
# is set (single-container shape from upstream's docker/entrypoint.sh).
# The dashboard's PID-based gateway-liveness check then works because
# both processes share this container's PID namespace.
ENTRYPOINT ["tini", "--", "/usr/local/bin/hermes-entrypoint.sh"]
CMD ["gateway"]
