# Hermes Agent container image.
#
# Upstream is github.com/NousResearch/hermes-agent. It ships as a Python
# package with a `hermes` console_script declared in pyproject.toml. We pin
# to a release tag rather than the install.sh script — the install script
# is opinionated about user-machine layout (~/.local/bin, venvs in $HOME)
# and unsuitable for a service container.
#
# Pinned to v2026.5.16 (upstream's latest date-tagged release as of
# 2026-05-16). Upstream ships date-tagged releases (`v<YYYY>.<M>.<D>`),
# not semver — see https://github.com/NousResearch/hermes-agent/tags.
# Bump deliberately; releases may touch Hermes' persistent SQLite schema
# only at boundaries upstream documents.
ARG HERMES_VERSION=v2026.5.16

FROM python:3.11-slim AS build

ARG HERMES_VERSION
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# git is needed for `pip install git+...`. Build tools support packages
# in the Hermes dep tree that may need to compile C extensions.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git \
      build-essential \
 && rm -rf /var/lib/apt/lists/*

# Install Hermes into /opt/venv so the runtime stage can copy it whole.
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"

# `[all]` pulls in the optional platform connectors (Telegram, Slack,
# Discord, etc.) defined in pyproject.toml's optional-dependencies. We
# install with `[all]` rather than per-platform extras so turning on a
# new platform later is a config change, not an image rebuild.
RUN pip install "hermes-agent[all] @ git+https://github.com/NousResearch/hermes-agent.git@${HERMES_VERSION}"

# ─── runtime stage ──────────────────────────────────────────────────────────
FROM python:3.11-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:${PATH}"

# tini for proper signal handling — Hermes' gateway is a long-running
# process and PID 1 needs to forward SIGTERM cleanly for graceful
# StatefulSet rollouts. Headless browser runtime deps live behind Hermes'
# `[browser]` extra; install chromium here if/when that path is exercised
# in-pod.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      tini \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 hermes \
 && mkdir -p /data \
 && chown -R hermes:hermes /data

COPY --from=build --chown=hermes:hermes /opt/venv /opt/venv

USER hermes
WORKDIR /home/hermes
VOLUME ["/data"]

# Hermes reads state from $HERMES_HOME / ~/.hermes by default. Point it at
# /data (the PVC mount) so SQLite + skills survive pod restarts. The
# StatefulSet's ConfigMap also exports HERMES_DATA_DIR pointing here; the
# image defaults are belt-and-suspenders for kubectl-exec / one-shot debug.
ENV HERMES_HOME=/data \
    HERMES_DATA_DIR=/data

EXPOSE 8000

# Long-running messaging gateway is the AKS-facing entrypoint. The
# interactive `hermes` CLI still works via `kubectl exec`. If upstream
# renames the gateway subcommand in a future release, this is the only
# line to update.
ENTRYPOINT ["tini", "--", "hermes"]
CMD ["gateway"]
