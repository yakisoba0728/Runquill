#!/bin/bash
# Shell tracing would expose the JIT credential.
set +ax
set -euo pipefail

jit_file="${RUNQUILL_JIT_FILE:-/run/runquill-jit}"
jit_wait=${RUNQUILL_JIT_WAIT:-}
if [[ -n "$jit_wait" && ! "$jit_wait" =~ ^[0-9]{1,10}$ ]]; then
    echo 'RUNQUILL_JIT_WAIT must be a nonnegative number of seconds' >&2
    exit 1
fi
jit_deadline=$((10#${jit_wait:-0}))
unset jit_config

dockerd_pid=
runner_pid=

# ShellCheck cannot see this call through the EXIT trap.
# shellcheck disable=SC2329
cleanup() {
    local pid
    trap '' INT TERM
    # The runner may still need Docker to clean up its job.
    for pid in "$runner_pid" "$dockerd_pid"; do
        [[ -n "$pid" ]] || continue
        kill -TERM "$pid" 2>/dev/null || :
        wait "$pid" || :
    done
}
trap 'cleanup' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# A warm runner starts before its job exists: it waits up to RUNQUILL_JIT_WAIT seconds for the
# manager to write the JIT config, and exits cleanly when none arrives.
read_jit() {
    while [[ ! -e "$jit_file" ]] && ((SECONDS < jit_deadline)); do
        if [[ -n "$dockerd_pid" ]]; then
            kill -0 "$dockerd_pid"
        fi
        sleep 0.2
    done
    if [[ ! -e "$jit_file" ]] && ((jit_deadline > 0)); then
        echo 'No job was handed to this warm runner' >&2
        exit 0
    fi
    if [[ ! -f "$jit_file" ]]; then
        echo 'JIT configuration file is missing or invalid' >&2
        exit 1
    fi
    jit_config=$(cat -- "$jit_file")
    rm -- "$jit_file"
    if [[ -z "$jit_config" ]]; then
        echo 'JIT configuration file is empty' >&2
        exit 1
    fi
}

if [[ -z "$jit_wait" ]]; then
    read_jit
fi
if [[ "${RUNQUILL_RUNNER_MODE:-dind}" == plain ]]; then
    if [[ -z "${jit_config:-}" ]]; then
        read_jit
    fi
    exec ./run.sh --jitconfig "$jit_config"
fi

# VFS works when the outer Docker host also uses an overlay filesystem.
docker_host=unix:///var/run/docker.sock
dockerd_args=(--host="$docker_host" --storage-driver=vfs)
# The manager or worker validated the URL; Docker uses a mirror only for Docker Hub images.
if [[ -n "${RUNQUILL_REGISTRY_MIRROR:-}" ]]; then
    dockerd_args+=("--registry-mirror=$RUNQUILL_REGISTRY_MIRROR")
fi
dockerd "${dockerd_args[@]}" &
dockerd_pid=$!

docker_deadline=$((SECONDS + 60))
while ((SECONDS < docker_deadline)); do
    kill -0 "$dockerd_pid"
    if timeout --kill-after=1s 2s docker --host="$docker_host" info >/dev/null 2>&1; then
        if [[ -z "${jit_config:-}" ]]; then
            read_jit
        fi
        # Drop privileges without a signal-relaying process between us and run.sh.
        HOME=/home/runner SHELL=/bin/bash USER=runner LOGNAME=runner \
            setpriv --reuid runner --regid runner --init-groups ./run.sh --jitconfig "$jit_config" &
        runner_pid=$!
        status=0
        wait "$runner_pid" || status=$?
        runner_pid=
        exit "$status"
    fi
    sleep 1
done

echo 'Docker did not become ready' >&2
exit 1
