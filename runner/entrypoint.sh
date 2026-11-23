#!/bin/bash
set -euo pipefail

jit_file="${RUNQUILL_JIT_FILE:-/run/runquill-jit}"
jit_config=

# A warm runner starts before its job exists: it waits up to RUNQUILL_JIT_WAIT seconds for the
# manager to write the JIT config, and exits cleanly when none arrives.
read_jit() {
    local deadline=${RUNQUILL_JIT_WAIT:-0}
    while [[ ! -e "$jit_file" ]] && ((SECONDS < deadline)); do
        if [[ -n "${dockerd_pid:-}" ]]; then
            kill -0 "$dockerd_pid"
        fi
        sleep 0.2
    done
    if [[ ! -e "$jit_file" && "$deadline" != 0 ]]; then
        echo 'No job was handed to this warm runner' >&2
        exit 0
    fi
    jit_config=$(cat "$jit_file")
    rm "$jit_file"
}
[[ -n "${RUNQUILL_JIT_WAIT:-}" ]] || read_jit

if [[ "${RUNQUILL_RUNNER_MODE:-dind}" == plain ]]; then
    [[ -n "$jit_config" ]] || read_jit
    exec ./run.sh --jitconfig "$jit_config"
fi

# VFS works when the outer Docker host also uses an overlay filesystem.
dockerd_args=(--host=unix:///var/run/docker.sock --storage-driver=vfs)
# The manager or worker validated the URL; Docker uses a mirror only for Docker Hub images.
if [[ -n "${RUNQUILL_REGISTRY_MIRROR:-}" ]]; then
    dockerd_args+=("--registry-mirror=$RUNQUILL_REGISTRY_MIRROR")
fi
dockerd "${dockerd_args[@]}" &
dockerd_pid=$!

for ((attempt = 0; attempt < 60; attempt++)); do
    kill -0 "$dockerd_pid"
    if docker info >/dev/null 2>&1; then
        [[ -n "$jit_config" ]] || read_jit
        # JIT runners exit after one job. Let the container exit with the runner.
        exec runuser -u runner -- ./run.sh --jitconfig "$jit_config"
    fi
    sleep 1
done

echo 'Docker did not become ready' >&2
exit 1
