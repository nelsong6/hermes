# TODO: real Hermes container build.
#
# Upstream installs via curl-to-Python script (https://hermes-agent.nousresearch.com/docs/).
# Real image needs to:
#   1. Pin a hermes-agent version (or git SHA) from NousResearch/hermes-agent.
#   2. Install Python 3.11 + dependencies (likely `pip install hermes-agent` once
#      they publish to PyPI; until then, install from git tag).
#   3. Create /data as the persistence root (PVC mounts here) and configure
#      Hermes to use it for SQLite + skills.
#   4. Run as non-root for the StatefulSet's pod security context.
#
# This file is a placeholder so the repo layout matches the rest of the AKS apps.
# Real implementation lands with the build pipeline + values.yaml image-tag wiring.
#
# Reference: NousResearch/hermes-agent

FROM python:3.11-slim

RUN useradd --create-home --uid 1000 hermes \
 && mkdir -p /data \
 && chown -R hermes:hermes /data

USER hermes
WORKDIR /home/hermes

# TODO: pin hermes-agent version and install. The curl one-liner the upstream
# install script uses is opinionated about the host environment; for the
# container path we want a direct pip install.
#
# RUN pip install --no-cache-dir hermes-agent==<pinned-version>
# OR
# RUN pip install --no-cache-dir 'hermes-agent @ git+https://github.com/NousResearch/hermes-agent@<sha>'

VOLUME ["/data"]

# Hermes' actual entrypoint command — confirm against upstream once the install
# is wired up. Likely `hermes serve` or similar.
# CMD ["hermes", "serve", "--data-dir", "/data"]
CMD ["sh", "-c", "echo 'hermes image is a stub — see Dockerfile TODOs' && sleep infinity"]
