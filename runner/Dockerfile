ARG RUNQUILL_RUNNER_BASE=ghcr.io/actions/actions-runner:latest@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300
FROM ${RUNQUILL_RUNNER_BASE}
LABEL org.opencontainers.image.licenses="Apache-2.0"

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends iptables kmod \
    && rm -rf /var/lib/apt/lists/*

COPY --chmod=755 entrypoint.sh /usr/local/bin/runquill-runner
COPY LICENSE /usr/share/licenses/runquill/LICENSE
ENTRYPOINT ["/usr/local/bin/runquill-runner"]
