#!/bin/sh
set -eu

repo=https://github.com/yakisoba0728/Runquill
tag=
directory=./runquill
webhook=
listen_port=
repository_scope=
organization=
runner_group_id=
download_only=false
upgrade=false
panel=false
wizard=false
panel_url=

usage() {
    cat <<'EOF_USAGE'
Usage: sh install.sh [--version vX.Y.Z] [--dir PATH] [--webhook-url URL] [--listen-port PORT] [--repository-scope all|selected] [--organization ORG [--runner-group-id ID]] [--download-only] [--wizard] [--panel [--panel-url URL]]
       sh install.sh --upgrade [--version vX.Y.Z] [--dir PATH]

  --version vX.Y.Z           Release to install (default: latest release).
  --dir PATH                 Install directory (default: ./runquill).
  --webhook-url URL          Subscribe the new GitHub App to webhooks at URL.
  --listen-port PORT         Local port for the GitHub App setup callback.
  --repository-scope SCOPE   Install the App on all or selected repositories.
  --organization ORG         Create the GitHub App under organization ORG.
  --runner-group-id ID       Register runners in this organization runner group.
  --download-only            Download and verify the release; skip setup and Compose.
  --wizard                   Download, then finish setup in the browser wizard.
  --panel                    Configure the web panel and start Compose with it.
  --panel-url URL            Browser origin of the panel (requires --panel).
  --upgrade                  Replace the release files of an existing install in
                             --dir after verifying checksums. Replaced files are
                             kept in .previous/. .env, keys and data are untouched.
  -h, --help                 Show this help.
EOF_USAGE
}

fail() {
    printf 'runquill install: %s\n' "$*" >&2
    exit 1
}

require() {
    command -v "$1" >/dev/null 2>&1 || fail "$1 is required but was not found. $2"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--dir|--webhook-url|--listen-port|--panel-url|--repository-scope|--organization|--runner-group-id)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then usage >&2; exit 2; fi
            case "$1" in
                --version) tag=$2 ;;
                --dir) directory=$2 ;;
                --webhook-url) webhook=$2 ;;
                --listen-port) listen_port=$2 ;;
                --panel-url) panel_url=$2 ;;
                --organization) organization=$2 ;;
                --runner-group-id) runner_group_id=$2 ;;
                --repository-scope)
                    case "$2" in all|selected) repository_scope=$2 ;; *) usage >&2; exit 2 ;; esac
                    ;;
            esac
            shift 2
            ;;
        --wizard) wizard=true; shift ;;
        --panel) panel=true; shift ;;
        --download-only) download_only=true; shift ;;
        --upgrade) upgrade=true; shift ;;
        -h|--help) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
done

if [ -n "$runner_group_id" ] && [ -z "$organization" ]; then
    printf '%s\n' '--runner-group-id requires --organization.' >&2
    exit 2
fi
if [ -n "$panel_url" ] && [ "$panel" = false ]; then
    printf '%s\n' '--panel-url requires --panel.' >&2
    exit 2
fi

if [ "$wizard" = true ] && { [ "$panel" = true ] || [ -n "$webhook$repository_scope$organization$runner_group_id" ]; }; then
    printf '%s\n' 'Set GitHub, panel, and webhook options inside the wizard.' >&2
    exit 2
fi

if [ "$upgrade" = true ] && { [ "$wizard" = true ] || [ "$panel" = true ] || [ -n "$webhook$listen_port$panel_url$repository_scope$organization$runner_group_id" ]; }; then
    printf '%s\n' '--upgrade only accepts --version and --dir; it never reruns setup.' >&2
    exit 2
fi

if [ "$(uname -s)" != Linux ]; then
    printf '%s\n' 'Run the installer on the Linux server that will run Runquill.' >&2
    exit 1
fi
case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) printf '%s\n' 'Runquill releases support Linux amd64 and arm64.' >&2; exit 1 ;;
esac

require curl 'Install curl with your package manager.'
require sha256sum 'Install GNU coreutils (or BusyBox) to verify checksums.'
for tool in awk cmp grep mktemp; do
    require "$tool" 'Install it with your package manager.'
done
if [ "$download_only" = false ] && [ "$wizard" = false ] && [ "$upgrade" = false ]; then
    require docker 'Install Docker Engine: https://docs.docker.com/engine/install/'
    docker compose version >/dev/null 2>&1 ||
        fail 'Docker Compose v2 is not available. Install the docker-compose-plugin package.'
    docker info >/dev/null 2>&1 ||
        fail 'The Docker daemon is not reachable. Start Docker, or run as a user allowed to use it (for example, a member of the docker group).'
fi
if [ -z "$tag" ]; then
    latest=$(curl --proto '=https' --proto-redir '=https' -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
    tag=${latest##*/}
fi
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$'; then
    printf '%s\n' 'Could not select a Runquill release tag. Specify --version vX.Y.Z.' >&2
    exit 1
fi

if [ "$upgrade" = true ]; then
    if [ ! -f "$directory/runquill" ] || [ -L "$directory/runquill" ]; then
        fail "No Runquill install found in $directory. Pass the existing install with --dir."
    fi
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
for optional in compose.panel.yaml compose.worker.yaml runquill-k3s.yaml; do
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
if [ "$upgrade" = true ]; then
    # Only release assets are replaced; .env, keys, data and anything else stay as they are.
    changed=
    for name in runquill $files; do
        asset=$name
        if [ "$name" = runquill ]; then asset=$binary; fi
        if [ -L "$name" ] || { [ -e "$name" ] && [ ! -f "$name" ]; }; then
            fail "$name is not a regular file. Move it away and rerun --upgrade."
        fi
        if [ ! -e "$name" ] || ! cmp -s "$scratch/$asset" "$name"; then
            changed="$changed $name"
        fi
    done
    if [ -z "$changed" ]; then
        printf 'Runquill %s is already installed in %s\n' "$tag" "$(pwd)"
        exit 0
    fi
    if [ -L .previous ] || { [ -e .previous ] && [ ! -d .previous ]; }; then
        fail '.previous is not a directory. Move it away and rerun --upgrade.'
    fi
    backup=$(mktemp -d .runquill-previous.XXXXXX)
    for name in $changed; do
        if [ -e "$name" ]; then cp -p -- "$name" "$backup/$name"; fi
    done
    rm -rf -- .previous
    mv -- "$backup" .previous
    for name in $changed; do
        asset=$name
        if [ "$name" = runquill ]; then asset=$binary; fi
        mv -f -- "$scratch/$asset" "$name"
    done
    printf 'Upgraded Runquill to %s in %s\n' "$tag" "$(pwd)"
    printf 'Replaced:%s\n' "$changed"
    printf '%s\n' 'Previous copies are in .previous/. To roll back: cp -p .previous/* .'
    printf '%s\n' 'Your .env, GitHub App key, and data were not changed.'
    manager_image=
    runner_image=
    if [ -f .env ]; then
        manager_image=$(awk -F= '$1 == "RUNQUILL_MANAGER_IMAGE" {print $2}' .env)
        runner_image=$(awk -F= '$1 == "RUNQUILL_RUNNER_IMAGE" {print $2}' .env)
    fi
    if [ -n "$manager_image" ] || [ -n "$runner_image" ]; then
        printf '%s\n' 'Point the images in .env at the new release:'
        if [ -n "$manager_image" ]; then printf '  RUNQUILL_MANAGER_IMAGE=%s:%s\n' "${manager_image%:*}" "$tag"; fi
        if [ -n "$runner_image" ]; then printf '  RUNQUILL_RUNNER_IMAGE=%s:%s\n' "${runner_image%:*}" "$tag"; fi
        printf '%s\n' 'Then restart Compose from this directory (add -f compose.yaml -f compose.panel.yaml, or your other overlays, if you started it with them):'
        if [ -n "$runner_image" ]; then printf '  docker pull %s:%s\n' "${runner_image%:*}" "$tag"; fi
        printf '%s\n' '  docker compose pull manager' '  docker compose up -d --no-build' '  docker compose exec -T manager runquill doctor'
    fi
    if [ -f runquill-k3s.yaml ]; then
        printf '%s\n' 'On K3s, apply the new manifest instead: kubectl apply -f runquill-k3s.yaml'
    fi
    printf 'Release notes: %s/releases/tag/%s\n' "$repo" "$tag"
    exit 0
fi
for name in runquill $files; do
    asset=$name
    if [ "$name" = runquill ]; then asset=$binary; fi
    if [ -e "$name" ] || [ -L "$name" ]; then
        if [ -L "$name" ] || ! cmp -s "$scratch/$asset" "$name"; then
            printf 'Existing %s differs. Choose an empty --dir for this release, or rerun with --upgrade to replace the release files.\n' "$name" >&2
            exit 1
        fi
    fi
done
for name in runquill $files; do
    asset=$name
    if [ "$name" = runquill ]; then asset=$binary; fi
    if [ ! -e "$name" ] && [ ! -L "$name" ]; then
        ln "$scratch/$asset" "$name"
    elif [ -L "$name" ] || ! cmp -s "$scratch/$asset" "$name"; then
        printf 'Existing %s changed during installation. Choose an empty --dir for this release.\n' "$name" >&2
        exit 1
    fi
done
printf 'Runquill %s is in %s\n' "$tag" "$(pwd)"
if [ "$download_only" = true ]; then exit 0; fi
if [ "$wizard" = true ]; then
    if [ ! -f compose.panel.yaml ] || ! ./runquill --help 2>&1 | grep -q 'runquill wizard'; then
        printf '%s\n' 'This release does not support browser setup. Select a newer release.' >&2
        exit 1
    fi
    set -- wizard
    if [ -n "$listen_port" ]; then set -- "$@" --listen-port "$listen_port"; fi
    ./runquill "$@"
    exit
fi

set -- setup
if [ -n "$webhook" ]; then set -- "$@" --webhook-url "$webhook"; fi
if [ -n "$listen_port" ]; then set -- "$@" --listen-port "$listen_port"; fi
if [ -n "$repository_scope" ]; then
    if ! ./runquill setup --help 2>&1 | grep -q -- '--repository-scope'; then
        printf '%s\n' 'This release does not support selecting repository scope. Select a newer release.' >&2
        exit 1
    fi
    set -- "$@" --repository-scope "$repository_scope"
fi
if [ -n "$organization" ]; then
    if ! ./runquill setup --help 2>&1 | grep -q -- '--organization'; then
        printf '%s\n' 'This release does not support organization setup. Select a newer release.' >&2
        exit 1
    fi
    set -- "$@" --organization "$organization"
    if [ -n "$runner_group_id" ]; then set -- "$@" --runner-group-id "$runner_group_id"; fi
fi
if [ "$panel" = true ]; then
    if [ ! -f compose.panel.yaml ] || ! ./runquill setup --help 2>&1 | grep -q -- '--panel'; then
        printf '%s\n' 'This release does not support guided panel setup. Select a newer release.' >&2
        exit 1
    fi
    set -- "$@" --panel
    if [ -n "$panel_url" ]; then set -- "$@" --panel-url "$panel_url"; fi
fi
./runquill "$@"
runner_image=$(awk -F= '$1 == "RUNQUILL_RUNNER_IMAGE" {print $2}' .env)
if ! printf '%s\n' "$runner_image" | grep -Eq '^ghcr\.io/[a-z0-9_./-]+:[A-Za-z0-9_.-]+$' ||
    [ "$(printf '%s\n' "$runner_image" | wc -l | tr -d ' ')" -ne 1 ]; then
    printf '%s\n' 'Setup did not provide a versioned runner image.' >&2
    exit 1
fi
set --
if [ "$panel" = true ]; then set -- -f compose.yaml -f compose.panel.yaml; fi
docker pull "$runner_image"
docker compose "$@" pull manager
docker compose "$@" up -d --no-build
docker compose "$@" exec -T manager runquill doctor
printf 'First job guide: %s/blob/%s/README.md#verify-your-first-job\n' "$repo" "$tag"
