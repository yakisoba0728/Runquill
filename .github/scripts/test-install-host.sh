#!/bin/sh
# Runs install.sh's host preparation for real in throwaway privileged containers:
# a non-root user with passwordless sudo installs Docker from the official
# repository, gets Docker access in the same run, and a second run changes nothing.
# Containers have no systemd, so the test starts dockerd itself when asked to.
#
# Usage: sh .github/scripts/test-install-host.sh IMAGE...   (for example ubuntu:24.04)
# Set RUNQUILL_TEST_WIZARD=1 to also download the latest release and start the
# wizard with a fake SSH_CONNECTION.
# Commands are single-quoted so they expand inside the container.
# shellcheck disable=SC2016
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
status=0
container=

cleanup() {
    if [ -n "$container" ]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

as_root() { docker exec "$container" sh -ec "$1"; }
as_user() { docker exec -u alice -w /home/alice "$container" sh -c "$1"; }

expect() { # LABEL EXPECTED-EXIT OUTPUT-PATTERN COMMAND
    printf -- '--- %s\n' "$1"
    set +e
    output=$(as_user "$4" 2>&1)
    code=$?
    set -e
    printf '%s\n' "$output" | sed 's/^/    | /'
    if [ "$code" -ne "$2" ]; then
        printf 'FAIL: %s exited %s, expected %s\n' "$1" "$code" "$2"
        return 1
    fi
    if [ -n "$3" ] && ! printf '%s\n' "$output" | grep -Eq -- "$3"; then
        printf 'FAIL: %s output does not match %s\n' "$1" "$3"
        return 1
    fi
    printf 'pass: %s\n' "$1"
}

start_dockerd() {
    docker exec -d "$container" sh -c 'dockerd >/var/log/dockerd.log 2>&1'
    tries=0
    until as_root 'docker info >/dev/null 2>&1'; do
        tries=$((tries + 1))
        if [ "$tries" -gt 60 ]; then as_root 'tail -n 30 /var/log/dockerd.log' || true; return 1; fi
        sleep 1
    done
}

run_image() {
    image=$1
    printf '\n===== %s\n' "$image"
    container=runquill-install-test-$$-$(printf '%s' "$image" | tr -c 'a-z0-9' -)
    docker run -d --privileged --name "$container" -v /var/lib/docker \
        -v "$root/install.sh:/src/install.sh:ro" "$image" sleep infinity >/dev/null
    case $image in
        alpine*) as_root 'apk add --no-cache sudo >/dev/null; adduser -D alice' ;;
        fedora*) as_root 'dnf install -y -q sudo >/dev/null; useradd -m alice' ;;
        *) as_root 'apt-get update -qq >/dev/null; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sudo >/dev/null; useradd -m alice' ;;
    esac
    as_root 'echo "alice ALL=(ALL) NOPASSWD:ALL" >/etc/sudoers.d/alice; chmod 440 /etc/sudoers.d/alice'

    expect 'without a terminal nothing is changed' 1 'no terminal to ask for consent' \
        'sh /src/install.sh --prepare-only'
    as_root '! command -v docker' || { printf 'FAIL: Docker was installed without consent\n'; return 1; }

    key=
    case $image in alpine*) ;; *) key='verified key https://download.docker.com/linux/[a-z]+/gpg' ;; esac
    expect 'install tools, Docker and group with --yes (no systemd: stop with a manual step)' 1 \
        "${key:-apk add --no-cache docker docker-cli-compose}" \
        'sh /src/install.sh --prepare-only --yes'
    as_user 'docker --version && docker compose version' | sed 's/^/    | /'

    start_dockerd
    # Drop the membership again so the next run has to add it and re-exec.
    case $image in alpine*) as_root 'delgroup alice docker' ;; *) as_root 'gpasswd -d alice docker >/dev/null' ;; esac

    expect 'add the docker group and continue in the same run' 0 'Continuing with the docker group active' \
        'sh /src/install.sh --prepare-only --yes --dir "/tmp/dir with spaces/it'\''s"'
    expect 'docker works for the user afterwards' 0 'Server' \
        'id -Gn; docker version; docker compose version'
    expect 'a healthy host is a no-op' 0 'Host is ready' \
        'out=$(sh /src/install.sh --prepare-only </dev/null 2>&1); s=$?; printf "%s\n" "$out"; if printf "%s" "$out" | grep -Eq "will run|fix  "; then exit 9; fi; exit $s'

    if [ "${RUNQUILL_TEST_WIZARD:-}" = 1 ]; then
        expect 'wizard over SSH prints the forwarding command' 0 'ssh -L 18080:127.0.0.1:18080 -p 2222 alice@10.0.0.9' \
            'SSH_CONNECTION="203.0.113.5 50000 10.0.0.9 2222" timeout 120 sh /src/install.sh --wizard --no-tailscale --listen-port 18080 --dir "$HOME/runquill" </dev/null >wizard.log 2>&1; cat wizard.log; grep -q "Open http://127.0.0.1:18080/?key=" wizard.log'
        expect '--upgrade runs the checks and keeps working' 0 'is already installed in' \
            'sh /src/install.sh --upgrade --dir "$HOME/runquill" --version "$("$HOME/runquill/runquill" --version)" </dev/null'
    fi
}

for image in "$@"; do
    if ! run_image "$image"; then
        status=1
        printf 'FAILED on %s\n' "$image"
    fi
    cleanup
    container=
done
exit "$status"
