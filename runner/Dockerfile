# One Dockerfile for every runner base. The Actions runner and the static Docker binaries always come
# from the digest-pinned upstream runner image, so every variant runs the same pinned versions.
# Each ARG defaults to a pinned stage below so it stays overridable while Dependabot, which reads only
# FROM lines, refreshes the digests.
ARG RUNQUILL_RUNNER_BASE=pinned-actions-runner
# ubuntu24.04 (the upstream image itself), ubuntu22.04, ubuntu26.04, debian12 or debian13.
ARG RUNQUILL_RUNNER_OS=ubuntu24.04
ARG RUNQUILL_UBUNTU2204=pinned-ubuntu22.04
ARG RUNQUILL_DEBIAN12=pinned-debian12
ARG RUNQUILL_UBUNTU2604=pinned-ubuntu26.04
ARG RUNQUILL_DEBIAN13=pinned-debian13

FROM ghcr.io/actions/actions-runner:latest@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300 AS pinned-actions-runner
FROM ubuntu:22.04@sha256:b000e69a50ff940390233025e7a337fb8469b509e2334fa4d018ac30f3e18cdb AS pinned-ubuntu22.04
FROM debian:12@sha256:1d04200c2ece1d4d75a2a6f8e11ebce7d86a2ae28b34bb2c9cbb3ee9f4953e75 AS pinned-debian12
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS pinned-ubuntu26.04
FROM debian:13@sha256:3cda3ca472ce45507a5109c6a3907093e24d9ae67d55954c9a07a45665dafba2 AS pinned-debian13

FROM ${RUNQUILL_RUNNER_BASE} AS actions

# Defined so every RUNQUILL_RUNNER_OS value names a stage; the ubuntu24.04 image never builds "ported".
FROM actions AS os-ubuntu24.04

FROM ${RUNQUILL_UBUNTU2204} AS os-ubuntu22.04
ENV ImageOS=ubuntu22

FROM ${RUNQUILL_DEBIAN12} AS os-debian12
ENV ImageOS=debian12

FROM ${RUNQUILL_UBUNTU2604} AS os-ubuntu26.04
ENV ImageOS=ubuntu26

FROM ${RUNQUILL_DEBIAN13} AS os-debian13
ENV ImageOS=debian13

# The upstream image uses Ubuntu mirrors over HTTP. HTTPS avoids failures on
# builders where port 80 stalls, and APT still verifies repository signatures.
FROM os-${RUNQUILL_RUNNER_OS} AS platform
USER root
RUN for source in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do \
        if [ -f "$source" ]; then \
            sed -Ei 's#http://((archive|security|ports)\.ubuntu\.com|deb\.debian\.org)/#https://\1/#g' "$source"; \
        fi; \
    done \
    && printf '%s\n' 'Acquire::Retries "3";' 'Acquire::https::Timeout "20";' \
        'APT::Update::Error-Mode "any";' >/etc/apt/apt.conf.d/99runquill-build

# Reproduces the upstream image's runner user, sudo rule and Docker group on another base.
FROM platform AS ported
ENV DEBIAN_FRONTEND=noninteractive RUNNER_MANUALLY_TRAP_SIG=1 ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
# The minimal bases need the upstream CA bundle only until ca-certificates is installed.
RUN --mount=type=bind,from=actions,source=/etc/ssl/certs/ca-certificates.crt,target=/tmp/runner-ca.crt \
    apt-get -o Acquire::https::CaInfo=/tmp/runner-ca.crt update \
    && apt-get -o Acquire::https::CaInfo=/tmp/runner-ca.crt install -y --no-install-recommends \
        ca-certificates curl git iptables jq kmod lsb-release sudo unzip \
    && groupadd --gid 123 docker \
    && groupadd --gid 1001 runner \
    && useradd --create-home --uid 1001 --gid 1001 --groups sudo,docker --shell /bin/bash runner \
    && echo "%sudo   ALL=(ALL:ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner \
    && echo "Defaults env_keep += \"DEBIAN_FRONTEND\"" >> /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner \
    && chmod 777 /home/runner \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /home/runner
COPY --from=actions --chown=runner:docker /home/runner/ /home/runner/
COPY --from=actions /usr/bin/docker /usr/bin/dockerd /usr/bin/docker-init /usr/bin/docker-proxy \
    /usr/bin/containerd /usr/bin/containerd-shim-runc-v2 /usr/bin/ctr /usr/bin/runc /usr/bin/
COPY --from=actions /usr/local/lib/docker/cli-plugins/ /usr/local/lib/docker/cli-plugins/
# The runner's own script installs the .NET native libraries (ICU, OpenSSL, Kerberos) for this distribution.
RUN ./bin/installdependencies.sh \
    && rm -rf /var/lib/apt/lists/* /etc/apt/apt.conf.d/99runquill-build

FROM platform AS runner-ubuntu24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends iptables kmod \
    && rm -rf /var/lib/apt/lists/* /etc/apt/apt.conf.d/99runquill-build

FROM ported AS runner-ubuntu22.04
FROM ported AS runner-debian12
FROM ported AS runner-ubuntu26.04
FROM ported AS runner-debian13

FROM runner-${RUNQUILL_RUNNER_OS}
LABEL org.opencontainers.image.licenses="Apache-2.0"

RUN test "$(id -u runner):$(id -g runner)" = 1001:1001

COPY --chmod=755 entrypoint.sh /usr/local/bin/runquill-runner
COPY LICENSE /usr/share/licenses/runquill/LICENSE
ENTRYPOINT ["/usr/local/bin/runquill-runner"]
