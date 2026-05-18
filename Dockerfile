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

# ─── stage 1b: build the embedded-chat TUI ─────────────────────────────
# The dashboard's Chat tab spawns a Node-based TUI subprocess via PTY —
# the same TUI you'd run from the CLI, rendered into the browser through
# xterm.js + a PTY bridge (`/api/pty`). pip install hermes-agent[all]
# does NOT bundle the ui-tui build artifacts in the wheel, so without
# this stage the runtime falls into the "Chat unavailable" branch with
# the misleading "npm not found" message that actually fires when `node`
# is missing.
#
# The TUI is structured as an npm workspace: ui-tui/ is the root,
# ui-tui/packages/hermes-ink/ is a sibling package referenced via
# "@hermes/ink": "file:./packages/hermes-ink" in the lockfile. No
# native deps, so node:20-alpine here is portable to the glibc-based
# runtime stage (we only ship the built dist/ + node_modules/, never the
# node binary from this stage).
FROM node:20-alpine AS tui-build
ARG HERMES_VERSION
RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 --branch ${HERMES_VERSION} \
      https://github.com/NousResearch/hermes-agent.git .
WORKDIR /src/ui-tui
# Matches upstream's docker/Dockerfile: install-as-copy for the
# file:packages/hermes-ink dep so we don't need to retain symlinks
# across the COPY into runtime. The dashboard's runtime npm-install
# fallback (`_tui_need_npm_install()`) re-fires every startup if the
# lockfile and node_modules disagree on link/copy mode; pinning it here
# matches the lockfile generator and keeps the runtime from trying to
# npm install (which would fail — we don't ship npm in the runtime).
ENV npm_config_install_links=false
RUN if [ -f package-lock.json ]; then npm ci --prefer-offline --no-audit; else npm install --prefer-offline --no-audit; fi \
 && npm run build \
 && test -f /src/ui-tui/dist/entry.js || (echo "expected /src/ui-tui/dist/entry.js after npm build" >&2; exit 1)

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

# tini for proper signal handling. nodejs (no npm) so the dashboard can
# spawn the prebuilt TUI from the tui-build stage when a user opens the
# Chat tab. Headless browser deps live behind Hermes' `[browser]`
# extra; install chromium here if/when that path is exercised in-pod.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      tini \
      ca-certificates \
      nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 hermes \
 && mkdir -p /data \
 && chown -R hermes:hermes /data

COPY --from=py-build --chown=hermes:hermes /opt/venv /opt/venv
# Prebuilt TUI from stage 1b. The dashboard's _make_tui_argv finds it
# via HERMES_TUI_DIR (set below) and spawns `node $dir/dist/entry.js`
# for every `/api/pty` WebSocket. We ship dist/ + node_modules/ +
# packages/ (the in-tree workspace dep) so the runtime never has to
# call npm — and we deliberately don't install npm in this stage.
COPY --from=tui-build --chown=hermes:hermes /src/ui-tui /opt/hermes/ui-tui
COPY --chown=hermes:hermes entrypoint.sh /usr/local/bin/hermes-entrypoint.sh
RUN chmod +x /usr/local/bin/hermes-entrypoint.sh

USER hermes
WORKDIR /home/hermes
VOLUME ["/data"]

ENV HERMES_HOME=/data \
    HERMES_DATA_DIR=/data \
    HERMES_TUI_DIR=/opt/hermes/ui-tui

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
