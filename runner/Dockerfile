FROM ghcr.io/actions/actions-runner:latest
LABEL org.opencontainers.image.licenses="Apache-2.0"

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends iptables kmod \
    && rm -rf /var/lib/apt/lists/*

COPY --chmod=755 entrypoint.sh /usr/local/bin/runquill-runner
COPY LICENSE /usr/share/licenses/runquill/LICENSE
ENTRYPOINT ["/usr/local/bin/runquill-runner"]
