#!/bin/sh
set -eu

repo=https://github.com/yakisoba0728/Runquill
tag=
directory=./runquill
webhook=
listen_port=
download_only=false

usage() {
    printf '%s\n' 'Usage: sh install.sh [--version vX.Y.Z] [--dir PATH] [--webhook-url URL] [--listen-port PORT] [--download-only]'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--dir|--webhook-url|--listen-port)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then usage >&2; exit 2; fi
            case "$1" in
                --version) tag=$2 ;;
                --dir) directory=$2 ;;
                --webhook-url) webhook=$2 ;;
                --listen-port) listen_port=$2 ;;
            esac
            shift 2
            ;;
        --download-only) download_only=true; shift ;;
        -h|--help) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
done

if [ "$(uname -s)" != Linux ]; then
    printf '%s\n' 'Run the installer on the Linux server that will run Runquill.' >&2
    exit 1
fi
case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) printf '%s\n' 'Runquill releases support Linux amd64 and arm64.' >&2; exit 1 ;;
esac

command -v curl >/dev/null
command -v sha256sum >/dev/null
if [ "$download_only" = false ]; then
    docker compose version >/dev/null
    docker info >/dev/null
fi
if [ -z "$tag" ]; then
    latest=$(curl --proto '=https' --proto-redir '=https' -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
    tag=${latest##*/}
fi
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$'; then
    printf '%s\n' 'Could not select a Runquill release tag. Specify --version vX.Y.Z.' >&2
    exit 1
fi

umask 077
mkdir -p -- "$directory"
cd -- "$directory"
scratch=$(mktemp -d .runquill-install.XXXXXX)
trap 'rm -rf -- "$scratch"' EXIT
binary=runquill-linux-$arch
url="$repo/releases/download/$tag"
files='compose.yaml compose.ssh.yaml LICENSE'
curl --proto '=https' --proto-redir '=https' -fsSL "$url/SHA256SUMS" -o "$scratch/SHA256SUMS"
for optional in compose.panel.yaml runquill-k3s.yaml; do
    if awk -v name="$optional" '$2 == name {found=1} END {exit !found}' "$scratch/SHA256SUMS"; then
        files="$files $optional"
    fi
done
for name in "$binary" $files; do
    curl --proto '=https' --proto-redir '=https' -fsSL "$url/$name" -o "$scratch/$name"
done
(
    cd "$scratch"
    for name in "$binary" $files; do
        checksum=$(awk -v name="$name" '$2 == name {print $1}' SHA256SUMS)
        if ! printf '%s\n' "$checksum" | grep -Eq '^[0-9a-f]{64}$' || [ "${#checksum}" -ne 64 ]; then
            printf 'Invalid or missing checksum for %s\n' "$name" >&2
            exit 1
        fi
        printf '%s  %s\n' "$checksum" "$name" | sha256sum -c -
    done
)
chmod 700 "$scratch/$binary"
for name in runquill $files; do
    asset=$name
    if [ "$name" = runquill ]; then asset=$binary; fi
    if [ -e "$name" ] || [ -L "$name" ]; then
        if [ -L "$name" ] || ! cmp -s "$scratch/$asset" "$name"; then
            printf 'Existing %s differs. Choose an empty --dir for this release.\n' "$name" >&2
            exit 1
        fi
    else
        ln "$scratch/$asset" "$name"
    fi
done
printf 'Runquill %s is in %s\n' "$tag" "$(pwd)"
if [ "$download_only" = true ]; then exit 0; fi

set -- setup
if [ -n "$webhook" ]; then set -- "$@" --webhook-url "$webhook"; fi
if [ -n "$listen_port" ]; then set -- "$@" --listen-port "$listen_port"; fi
./runquill "$@"
runner_image=$(awk -F= '$1 == "RUNQUILL_RUNNER_IMAGE" {print $2}' .env)
if ! printf '%s\n' "$runner_image" | grep -Eq '^ghcr\.io/[a-z0-9_./-]+:[A-Za-z0-9_.-]+$' ||
    [ "$(printf '%s\n' "$runner_image" | wc -l | tr -d ' ')" -ne 1 ]; then
    printf '%s\n' 'Setup did not provide a versioned runner image.' >&2
    exit 1
fi
docker pull "$runner_image"
docker compose pull manager
docker compose up -d --no-build
docker compose exec -T manager runquill doctor
printf 'First job guide: %s/blob/%s/README.md#verify-your-first-job\n' "$repo" "$tag"
