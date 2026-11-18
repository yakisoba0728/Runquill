#!/bin/bash
set -euo pipefail

jit_config=$(cat /run/runquill-jit)
rm /run/runquill-jit

# VFS works when the outer Docker host also uses an overlay filesystem.
dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs &
dockerd_pid=$!

for ((attempt = 0; attempt < 60; attempt++)); do
    kill -0 "$dockerd_pid"
    if docker info >/dev/null 2>&1; then
        # JIT runners exit after one job. Let the container exit with the runner.
        exec runuser -u runner -- ./run.sh --jitconfig "$jit_config"
    fi
    sleep 1
done

echo 'Docker did not become ready' >&2
exit 1
