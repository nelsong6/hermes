#!/bin/sh
# Hermes container entrypoint.
#
# Match upstream's single-container layout (`docker/entrypoint.sh` in
# NousResearch/hermes-agent): if HERMES_DASHBOARD=1, background
# `hermes dashboard ...` inside this container before exec'ing the main
# command (typically `gateway`). Same PID namespace means the dashboard's
# gateway-liveness check (gateway.pid + flock on gateway.lock under
# $HERMES_HOME) sees the gateway process correctly.
#
# Docs (website/docs/user-guide/docker.md):
#   "Running [the dashboard] as a separate container is not supported:
#    the dashboard's gateway-liveness detection requires a shared PID
#    namespace with the gateway process."
#
# The dashboard is intentionally unsupervised — upstream explicitly
# doesn't restart it on crash. If we want supervision later, wrap it
# (s6-overlay, supervisord) — but matching upstream's shape first.

set -eu

if [ "${HERMES_DASHBOARD:-0}" = "1" ]; then
    dashboard_args="--host ${HERMES_DASHBOARD_HOST:-127.0.0.1} --port ${HERMES_DASHBOARD_PORT:-9119}"
    if [ "${HERMES_DASHBOARD_TUI:-0}" = "1" ]; then
        dashboard_args="${dashboard_args} --tui"
    fi
    echo "entrypoint: backgrounding 'hermes dashboard ${dashboard_args}'" >&2
    # shellcheck disable=SC2086
    hermes dashboard ${dashboard_args} &
fi

# Foreground process — tini (PID 1) forwards SIGTERM here for graceful
# rollouts. The backgrounded dashboard gets reaped when this exits.
exec hermes "$@"
