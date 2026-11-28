#!/bin/sh
# Offline tests for install.sh's host checks and release handling. Every system command the
# installer touches (uname, id, docker, sudo, apt-get, systemctl, curl, gpg, sg,
# ...) is a stub on PATH that records its arguments, and /etc, /proc, /run and /sys
# come from a fake root, so nothing on this machine is read or changed.
#
# Usage: sh .github/scripts/test-install-offline.sh [SHELL]   (default: sh)
# Linux only (the stubs link real coreutils); TTY cases need util-linux `script`.
# The stub bodies are single-quoted on purpose; option lists are split on purpose.
# shellcheck disable=SC2016,SC2086
set -eu

shell_under_test=${1:-sh}
installer=$(cd "$(dirname "$0")/../.." && pwd)/install.sh
work=$(mktemp -d)
trap 'rm -rf -- "$work"' 0
passed=0
failed=0

# ---------------------------------------------------------------- stubs

mkdir -p "$work/real" "$work/templates"
for tool in sh dash bash ash awk sed grep cat mktemp rm mkdir rmdir tr cut head tail readlink cmp wc env chmod ln cp mv \
    sleep tee dirname basename sha256sum timeout touch ls printf test true false expr sort; do
    path=$(command -v "$tool" 2>/dev/null) || continue
    case $path in /*) ln -s "$path" "$work/real/$tool" ;; esac
done

stub() { # NAME BODY: writes a template stub that logs its arguments
    {
        printf '#!/bin/sh\n'
        printf 'printf "%%s\\n" "%s $*" >>"$STUB_STATE/log"\n' "$1"
        printf '%s\n' "$2"
    } >"$work/templates/$1"
    chmod 755 "$work/templates/$1"
}

stub uname 'case $1 in -s) echo "${STUB_KERNEL:-Linux}" ;; -m) echo "${STUB_MACHINE:-x86_64}" ;; *) echo Linux ;; esac'
stub id 'case "$*" in
    -u) echo "${STUB_UID:-1000}" ;;
    -un) echo "${STUB_USER:-alice}" ;;
    -Gn) if [ -e "$STUB_STATE/process-docker" ]; then STUB_GROUPS="${STUB_GROUPS:-} docker"; fi
        echo "${STUB_USER:-alice} ${STUB_GROUPS:-}" ;;
    "-Gn "*) if [ -e "$STUB_STATE/in-docker-group" ]; then echo "${STUB_USER:-alice} docker"; else echo "${STUB_USER:-alice}"; fi ;;
esac'
stub stat 'echo "${STUB_CGROUP:-cgroup2fs}"'
stub df 'echo "Filesystem 1024-blocks Used Available Capacity Mounted"; echo "/dev/x 1 1 ${STUB_DISK_KB:-104857600} 1% /"'
stub curl 'out=; write=; prev=; url=
for a in "$@"; do
    if [ "$prev" = -o ]; then
        out=$a
        case $url in */releases/download/*)
            if [ -d "$STUB_STATE/release" ]; then
                cp "$STUB_STATE/release/${url##*/}" "$out" || exit 22
            fi ;;
        esac
    fi
    if [ "$prev" = -w ]; then write=1; fi
    case $a in https://*) url=$a ;; esac
    prev=$a
done
if [ -n "$write" ]; then printf "%s" "${STUB_HTTP:-200}"; [ "${STUB_HTTP:-200}" != 000 ] || exit 7; exit 0; fi
[ "${STUB_HTTP:-200}" != 000 ] || exit 7
if [ "$url" = https://tailscale.com/install.sh ]; then cp "$STUB_TEMPLATES/tailscale-install" "$out"; exit; fi
if [ -n "$out" ] && [ "$out" != /dev/null ] && [ ! -d "$STUB_STATE/release" ]; then echo stub-download >"$out"; fi'
stub mv 'if [ "$#" -eq 3 ] && [ "${2##*/}" = backup ] && [ "$3" = .previous ]; then exit 1; fi
exec "$STUB_TEMPLATES/../real/mv" "$@"'
stub mkdir '"$STUB_TEMPLATES/../real/mkdir" "$@" || exit "$?"
if [ "$*" = .runquill-upgrade ]; then
    kill -TERM "$(cat "$STUB_STATE/installer-pid")"
else
    printf "%s\n" "$PPID" >"$STUB_STATE/installer-pid"
fi'
stub gpg 'printf "pub:-:4096:1:X:::::\nfpr:::::::::%s:\nsub:-:4096:1:Y:::::\nfpr:::::::::%s:\n" "${STUB_FPR:-9DC858229FC7DD38854AE2D88D81803C0EBFCD88}" D3306A018370199E527AE7997EA0A9C3F273FCD8'
stub sudo 'if [ -n "${STUB_SUDO_BROKEN:-}" ]; then exit 1; fi
if [ "$1" = -n ]; then [ -z "${STUB_SUDO_PASSWORD:-}" ]; exit; fi
if [ "$1" = -u ]; then shift 2; STUB_GROUPS="${STUB_GROUPS:+$STUB_GROUPS }docker"; export STUB_GROUPS; exec "$@"; fi
STUB_UID=0 exec "$@"'
stub install ':'
stub groupadd ':'
stub rpm ':'
stub usermod 'touch "$STUB_STATE/in-docker-group"'
stub addgroup 'touch "$STUB_STATE/in-docker-group"'
stub getent 'exit 0'
stub dpkg 'echo amd64'
stub dpkg-query 'if [ -e "$STUB_STATE/docker-ce" ]; then printf "install ok installed"; fi'
stub systemctl 'case $1 in
    is-enabled) [ -e "$STUB_STATE/enabled" ] ;;
    enable)
        if [ "$*" = "enable --now tailscaled" ]; then touch "$STUB_STATE/ts-up"
        else touch "$STUB_STATE/enabled"; if [ "$2" = --now ]; then touch "$STUB_STATE/daemon-up"; fi; fi ;;
esac'
stub sg 'eval "set -- $3"
shift 3
for a in "$@"; do printf "arg[%s]\n" "$a" >>"$STUB_STATE/log"; done
if [ -z "${STUB_SG_NOOP:-}" ]; then STUB_GROUPS="${STUB_GROUPS:+$STUB_GROUPS }docker"; export STUB_GROUPS; fi
exec sh "$INSTALLER_COPY" "$@"'
stub docker 'case "$*" in
    --version) echo "Docker version 27.5.1, build test" ;;
    "compose version"*) [ -e "$STUB_STATE/compose" ] && echo 2.30.0 ;;
    info*)
        if [ ! -e "$STUB_STATE/daemon-up" ]; then echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock" >&2; exit 1; fi
        if [ -e "$STUB_STATE/process-docker" ]; then STUB_GROUPS="${STUB_GROUPS:-} docker"; fi
        case " ${STUB_GROUPS:-} " in *" docker "*) ;; *)
            if [ "${STUB_UID:-1000}" != 0 ]; then echo "permission denied while trying to connect to the Docker daemon socket" >&2; exit 1; fi ;;
        esac
        case "$*" in
            *DockerRootDir*) echo /var/lib/docker ;;
            *SecurityOptions*) echo "${STUB_SECOPTS:-[\"name=seccomp\"]}" ;;
        esac ;;
esac'
stub k3s ':'
stub tailscale 'case "$1 $2" in
    "status --json")
        [ -z "${STUB_TAILSCALE_DOWN:-}" ] || [ -e "$STUB_STATE/ts-up" ] || exit 1
        state=${STUB_TAILSCALE_STATE:-Running}
        if [ -e "$STUB_STATE/ts-signed-in" ]; then state=Running; fi
        printf "{\n  \"BackendState\": \"%s\"\n}\n" "$state" ;;
    "debug prefs")
        [ -z "${STUB_TAILSCALE_PREFS_FAIL:-}" ] || exit 1
        operator=${STUB_TAILSCALE_OPERATOR:-}
        if [ -e "$STUB_STATE/ts-operator" ]; then operator=alice; fi
        printf "{\n  \"OperatorUser\": \"%s\"\n}\n" "$operator" ;;
    set\ --operator=*) touch "$STUB_STATE/ts-operator" ;;
    up\ --timeout=*) touch "$STUB_STATE/ts-signed-in" ;;
esac'
stub tailscale-install '
    [ -z "${STUB_TAILSCALE_INSTALL_FAIL:-}" ] || exit 1
    cp "$STUB_TEMPLATES/tailscale" "$STUB_BIN/"
    touch "$STUB_STATE/ts-up"'
# Package managers: installing a package puts the matching stubs on PATH.
pm_body='for p in "$@"; do
    case $p in
        curl) cp "$STUB_TEMPLATES/curl" "$STUB_BIN/" ;;
        gpg|gnupg|gnupg2) cp "$STUB_TEMPLATES/gpg" "$STUB_BIN/" ;;
        docker-ce|docker) cp "$STUB_TEMPLATES/docker" "$STUB_BIN/"; touch "$STUB_STATE/docker-ce" "$STUB_STATE/compose" ;;
        docker-compose-plugin|docker-cli-compose) [ -n "${STUB_COMPOSE_INSTALL_NOOP:-}" ] || touch "$STUB_STATE/compose" ;;
    esac
done
[ -z "${STUB_PM_FAIL:-}" ]'
for pm in apt-get dnf apk; do stub "$pm" "$pm_body"; done

# ---------------------------------------------------------------- hosts

# host [docker] [up] [group|dbgroup] [enabled] [compose] [systemd] [k3s] [nocurl] [nosudo] [os=ID]
host() {
    case_dir=$work/case
    rm -rf -- "$case_dir"
    mkdir -p "$case_dir/bin" "$case_dir/state" "$case_dir/root/etc" "$case_dir/root/proc" "$case_dir/home"
    : >"$case_dir/state/log"
    cp "$installer" "$case_dir/install.sh"
    os=ubuntu pm=apt-get
    for name in uname id stat df curl gpg sudo install groupadd rpm usermod getent dpkg dpkg-query systemctl sg; do
        cp "$work/templates/$name" "$case_dir/bin/"
    done
    for option in "$@"; do
        case $option in
            docker) cp "$work/templates/docker" "$case_dir/bin/"; touch "$case_dir/state/docker-ce" ;;
            up) touch "$case_dir/state/daemon-up" ;;
            group) touch "$case_dir/state/in-docker-group" "$case_dir/state/process-docker" ;;
            dbgroup) touch "$case_dir/state/in-docker-group" ;;
            enabled) touch "$case_dir/state/enabled" ;;
            compose) touch "$case_dir/state/compose" ;;
            systemd) mkdir -p "$case_dir/root/run/systemd/system" ;;
            k3s) cp "$work/templates/k3s" "$case_dir/bin/" ;;
            tailscale) cp "$work/templates/tailscale" "$case_dir/bin/" ;;
            tsoperator) touch "$case_dir/state/ts-operator" ;;
            nocurl) rm "$case_dir/bin/curl" ;;
            nogpg) rm "$case_dir/bin/gpg" ;;
            nosudo) rm "$case_dir/bin/sudo" ;;
            os=*) os=${option#os=} ;;
        esac
    done
    case $os in
        ubuntu) printf 'ID=ubuntu\nVERSION_ID="24.04"\nVERSION_CODENAME=noble\nUBUNTU_CODENAME=noble\nPRETTY_NAME="Ubuntu 24.04 LTS"\n' ;;
        debian) printf 'ID=debian\nVERSION_ID="12"\nVERSION_CODENAME=bookworm\nPRETTY_NAME="Debian GNU/Linux 12 (bookworm)"\n' ;;
        fedora) pm=dnf; printf 'ID=fedora\nVERSION_ID=42\nPRETTY_NAME="Fedora Linux 42"\n' ;;
        alpine) pm=apk; printf 'ID=alpine\nVERSION_ID=3.23.0\nPRETTY_NAME="Alpine Linux v3.23"\n' ;;
        arch) pm=; printf 'ID=arch\nPRETTY_NAME="Arch Linux"\n' ;;
    esac >"$case_dir/root/etc/os-release"
    if [ -n "$pm" ]; then cp "$work/templates/$pm" "$case_dir/bin/"; fi
}

# run [ENV=VALUE...] -- ARGS...: runs the installer without a terminal.
run() {
    run_env=
    while [ "$1" != -- ]; do run_env="$run_env $1"; shift; done
    shift
    set +e
    # shellcheck disable=SC2086 # run_env is a list of VAR=value words
    output=$(cd "$case_dir/home" && env -i HOME="$case_dir/home" PATH="$case_dir/bin:$work/real" \
        RUNQUILL_INSTALL_SYSROOT="$case_dir/root" STUB_STATE="$case_dir/state" STUB_BIN="$case_dir/bin" \
        STUB_TEMPLATES="$work/templates" INSTALLER_COPY="$case_dir/install.sh" TMPDIR="$case_dir" $run_env \
        "$shell_under_test" "$case_dir/install.sh" "$@" </dev/null 2>&1)
    code=$?
    set -e
}

# run_tty ANSWER ARGS...: runs the installer on a pseudo-terminal and types ANSWER.
run_tty() {
    answer=$1
    shift
    if [ -n "${STUB_STREAMED:-}" ]; then
        command_line="cat '$case_dir/install.sh' | $shell_under_test -s --"
    else
        command_line="$shell_under_test '$case_dir/install.sh'"
    fi
    for a in "$@"; do command_line="$command_line '$a'"; done
    set +e
    output=$(cd "$case_dir/home" && printf '%s\n' "$answer" | env -i HOME="$case_dir/home" PATH="$case_dir/bin:$work/real" \
        RUNQUILL_INSTALL_SYSROOT="$case_dir/root" STUB_STATE="$case_dir/state" STUB_BIN="$case_dir/bin" \
        STUB_TEMPLATES="$work/templates" INSTALLER_COPY="$case_dir/install.sh" TMPDIR="$case_dir" \
        STUB_TAILSCALE_STATE="${STUB_TAILSCALE_STATE:-}" \
        "$script_bin" -qec "$command_line" /dev/null 2>&1)
    code=$?
    set -e
}

check() { # NAME CONDITION...
    check_name=$1
    shift
    if "$@"; then
        passed=$((passed + 1))
    else
        failed=$((failed + 1))
        printf 'FAIL: %s (exit %s)\n' "$check_name" "$code"
        printf '%s\n' "$output" | sed 's/^/    | /'
        printf '    log:\n'
        sed 's/^/    > /' "$case_dir/state/log"
    fi
}
has_out() { printf '%s\n' "$output" | grep -Fq -- "$1"; }
lacks_out() { ! has_out "$1"; }
has_log() { grep -Fq -- "$1" "$case_dir/state/log"; }
lacks_log() { ! has_log "$1"; }
exit_is() { [ "$code" -eq "$1" ]; }
no_matches() { [ "$#" -eq 1 ] && [ ! -e "$1" ]; }
changed_nothing() {
    ! grep -Eq '^(apt-get|dnf|apk|install|usermod|addgroup|rpm|groupadd|systemctl enable|sg) ' "$case_dir/state/log"
}

release() {
    fixture=$case_dir/state/release
    mkdir "$fixture"
    (
        cd "$fixture"
        for asset in runquill-linux-amd64 compose.yaml compose.ssh.yaml compose.panel.yaml runquill-k3s.yaml LICENSE; do
            printf 'new %s\n' "$asset" >"$asset"
            sha256sum "$asset"
        done >SHA256SUMS
    )
}

existing_install() { # [.env LINE...]
    installed=$case_dir/home/runquill
    mkdir -p "$installed/.previous"
    printf '%s\n' 'old binary' >"$installed/runquill"
    printf '%s\n' 'old compose' >"$installed/compose.yaml"
    printf '%s\n' 'older binary' >"$installed/.previous/runquill"
    printf '%s\n' 'FIXTURE=keep' "$@" >"$case_dir/original.env"
    cp "$case_dir/original.env" "$installed/.env"
}

healthy='docker up group enabled compose systemd'

# ---------------------------------------------------------------- cases

host $healthy
run -- --prepare-only
check 'healthy host: ready' exit_is 0
check 'healthy host: no plan' lacks_out 'will run'
check 'healthy host: nothing changed' changed_nothing
check 'healthy host: checklist' has_out 'ok    Docker access  alice can use the daemon'
host $healthy tailscale
run -- --prepare-only --no-install
check 'read-only host check accepts missing Tailscale operator' exit_is 0
check 'read-only host check leaves Tailscale alone' lacks_log 'tailscale'

# This harness stops at the release manifest; reaching it means host prep continued.
host $healthy tailscale
run -- --wizard --version v0.3.2 --no-install
check 'read-only wizard can download and start later' has_log 'v0.3.2/SHA256SUMS'
check 'read-only wizard leaves Tailscale alone' lacks_log 'tailscale'

host $healthy tailscale
run -- --wizard --version v0.3.2
check 'no TTY: optional Tailscale setup does not block the wizard' has_log 'v0.3.2/SHA256SUMS'
check 'no TTY: optional Tailscale setup needs consent' lacks_log 'tailscale set'
run -- --wizard --version v0.3.2 --yes
check 'wizard --yes configures an unassigned operator' has_log 'tailscale set --operator=alice'

host $healthy tailscale
run STUB_TAILSCALE_OPERATOR=bob -- --wizard --version v0.3.2 --yes
check 'wizard preserves a different Tailscale operator' lacks_log 'tailscale set'
check 'wizard does not reauthenticate a running node' lacks_log 'tailscale up'

host $healthy tailscale tsoperator
run -- --wizard --version v0.3.2 --yes
check 'wizard preserves the current Tailscale operator' lacks_log 'tailscale set'

host $healthy tailscale
run STUB_TAILSCALE_PREFS_FAIL=1 -- --wizard --version v0.3.2 --yes
check 'wizard preserves the operator when preferences cannot be read' lacks_log 'tailscale set'

host $healthy
run STUB_TAILSCALE_STATE=NeedsLogin -- --wizard --version v0.3.2 --yes
check 'wizard installs Tailscale after consent' has_log 'https://tailscale.com/install.sh'
check 'wizard configures the operator of the newly installed node' has_log 'tailscale set --operator=alice'
check 'headless wizard leaves Tailscale account sign-in to the user' lacks_log 'tailscale up'

host $healthy
run STUB_TAILSCALE_INSTALL_FAIL=1 -- --wizard --version v0.3.2 --yes
check 'optional Tailscale install failure falls back to polling' has_out 'Runquill can find jobs by polling'
check 'optional Tailscale install failure continues to release' has_log 'v0.3.2/SHA256SUMS'

host $healthy
run -- --wizard --version v0.3.2 --yes --no-tailscale
check 'wizard can opt out of Tailscale' lacks_log 'tailscale'
check 'Tailscale opt-out continues to release' has_log 'v0.3.2/SHA256SUMS'

host $healthy tailscale
run STUB_TAILSCALE_DOWN=1 -- --wizard --version v0.3.2 --yes
check 'wizard starts an installed Tailscale service' has_log 'systemctl enable --now tailscaled'

host systemd k3s
run -- --wizard --version v0.3.2 --yes
check 'wizard skips managed Tailscale on K3s-only hosts' lacks_log 'tailscale'

host systemd
run -- --prepare-only --dir "/srv/run quill/it's"
check 'non-TTY: refuses' exit_is 1
check 'non-TTY: nothing changed' changed_nothing
check 'non-TTY: lists apt repo' has_out '/etc/apt/sources.list.d/docker.sources'
check 'non-TTY: names fingerprint' has_out '9DC858229FC7DD38854AE2D88D81803C0EBFCD88'
check 'non-TTY: warns about root-equivalent group' has_out 'root-equivalent access'
check 'non-TTY: prints rerun command with quoted args' has_out "'--prepare-only' '--dir' '/srv/run quill/it'\\''s' --yes"

host systemd
run -- --prepare-only --no-install
check '--no-install: fails' exit_is 1
check '--no-install: message' has_out 'Nothing was changed (--no-install)'
check '--no-install: nothing changed' changed_nothing

host systemd
run -- --yes --no-install
check '--yes with --no-install is a usage error' exit_is 2

host systemd nocurl nogpg
run -- --prepare-only --yes --dir "/srv/run quill/it's"
check '--yes: completes' exit_is 0
check '--yes: tools installed' has_log 'apt-get install -y curl ca-certificates gpg'
check '--yes: keyring' has_log 'install -m 0644'
check '--yes: verified key' has_out "verified key https://download.docker.com/linux/ubuntu/gpg (fingerprint 9DC858229FC7DD38854AE2D88D81803C0EBFCD88)"
check '--yes: docker packages' has_log 'apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin'
check '--yes: daemon started' has_log 'systemctl enable --now docker'
check '--yes: group added' has_log 'usermod -aG docker alice'
check '--yes: sudo used for root commands' has_log 'sudo env DEBIAN_FRONTEND=noninteractive apt-get update'
check '--yes: re-exec with sg' has_log 'sg docker -c exec /bin/sh'
check '--yes: re-exec keeps args with spaces and quotes' has_log "arg[/srv/run quill/it's]"
check '--yes: re-exec keeps --yes' has_log 'arg[--yes]'
check '--yes: ready after re-exec' has_out 'Host is ready.'
check '--yes: re-login note' has_out 'Shells opened before now need a new login'

host systemd
run STUB_FPR=0000000000000000000000000000000000000000 -- --prepare-only --yes
check 'bad key: fails' exit_is 1
check 'bad key: says so' has_out "not Docker's published 9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
check 'bad key: key not installed' lacks_log '/etc/apt/keyrings/docker.asc'
check 'bad key: no docker packages' lacks_log 'docker-ce'
check 'bad key: reports partial state' has_out 'failed while: install Docker Engine'

host systemd
run STUB_PM_FAIL=1 -- --prepare-only --yes
check 'package failure: reports what changed' has_out 'Already changed on this host'
check 'package failure: exit 1' exit_is 1

host $healthy
run STUB_MACHINE=riscv64 -- --prepare-only
check 'riscv64: refused' exit_is 1
check 'riscv64: explained' has_out 'only for Linux amd64 (x86_64) and arm64 (aarch64), not riscv64'

host $healthy
run STUB_KERNEL=Darwin -- --prepare-only
check 'macOS: refused' has_out 'Run the installer on that server'

host $healthy
run STUB_CGROUP=tmpfs -- --prepare-only --yes
check 'cgroup v1: refused' exit_is 1
check 'cgroup v1: explained' has_out 'systemd.unified_cgroup_hierarchy=1'
check 'cgroup v1: nothing changed' changed_nothing

host $healthy
run 'STUB_SECOPTS=["name=seccomp","name=rootless"]' -- --prepare-only --yes
check 'rootless: refused' exit_is 1
check 'rootless: explained' has_out 'runs rootless'
check 'rootless: nothing changed' changed_nothing

host systemd os=arch
run -- --prepare-only --yes
check 'unknown distro: refused' exit_is 1
check 'unknown distro: docs link' has_out 'https://docs.docker.com/engine/install/'
check 'unknown distro: nothing changed' changed_nothing

host systemd os=fedora
run -- --prepare-only
check 'fedora: rpm key' has_out "Verify Docker's signing key"
check 'fedora: repo file' has_out '/etc/yum.repos.d/docker-ce.repo'
check 'fedora: dnf install' has_out 'sudo dnf install -y docker-ce'
check 'fedora: rpm fingerprint' has_out '060A61C51B558A7F742B77AAC52FEB6B621E9F35'

host systemd os=fedora
run STUB_HTTP=404 -- --prepare-only --yes
check 'repo without this release: refused' has_out "Docker's repository does not offer Fedora Linux 42"
check 'repo without this release: nothing changed' changed_nothing

host os=alpine
run -- --prepare-only
check 'alpine: community packages' has_out "Docker's repository does not support Alpine"
check 'alpine: apk command' has_out 'sudo apk add --no-cache docker docker-cli-compose'
check 'alpine: addgroup' has_out 'sudo addgroup alice docker'

host systemd
run STUB_UID=0 STUB_USER=root -- --prepare-only --yes
check 'root: completes' exit_is 0
check 'root: no sudo' lacks_log 'sudo env'
check 'root: no group change' lacks_log 'usermod'

host systemd nosudo
run -- --prepare-only --yes
check 'no sudo: refused' has_out 'sudo is not installed'
check 'no sudo: nothing changed' changed_nothing

host systemd
run STUB_SUDO_PASSWORD=1 -- --prepare-only --yes
check 'sudo password without terminal: refused' has_out 'no terminal to ask for it'
check 'sudo password without terminal: nothing changed' changed_nothing

host docker group compose systemd
run -- --prepare-only --yes
check 'daemon down (systemd): started' has_log 'systemctl enable --now docker'
check 'daemon down (systemd): ready' exit_is 0

host docker group compose
run -- --prepare-only --yes
check 'daemon down (no systemd): manual step' has_out 'no systemd, so it is not started automatically'
check 'daemon down (no systemd): exit 1' exit_is 1
check 'daemon down (no systemd): nothing started' lacks_log 'systemctl enable'

host docker up group compose systemd
run -- --prepare-only --yes
check 'not enabled: enabled' has_log 'systemctl enable docker'

host docker up group enabled systemd
run -- --prepare-only --yes
check 'compose missing: plugin installed' has_log 'apt-get install -y docker-compose-plugin'
check 'compose missing: ready' exit_is 0

host docker up group enabled systemd
run STUB_COMPOSE_INSTALL_NOOP=1 -- --prepare-only --yes
check 'compose still missing after install: fails' exit_is 1
check 'compose still missing after install: rechecked' has_out 'The host is still not ready'

host docker up group enabled systemd nosudo
run -- --prepare-only --yes
check 'compose missing without sudo: fails' exit_is 1
check 'compose missing without sudo: explains the permission issue' has_out 'Installing Docker Compose v2: this needs root'

host systemd k3s
run -- --version v0.3.2
check 'direct setup on K3s without Compose: fails' exit_is 1
check 'direct setup on K3s without Compose: suggests wizard' has_out 'use --wizard for K3s'
check 'direct setup on K3s without Compose: fails before download' lacks_log '/SHA256SUMS'

host docker up dbgroup enabled compose systemd
run -- --prepare-only
check 'group already in db: no consent needed' exit_is 0
check 'group already in db: re-exec' has_log 'sg docker -c'
check 'group already in db: no change' lacks_log 'usermod'

host docker up dbgroup enabled compose systemd
newline='
'
run -- --prepare-only --dir "trailing${newline}"
check 'group re-exec keeps a trailing newline in an argument' grep -Fxq 'arg[trailing' "$case_dir/state/log"

host docker up dbgroup enabled compose systemd
run STUB_SG_NOOP=1 -- --prepare-only
check 'group still inactive: fails' exit_is 1
check 'group still inactive: re-login advice' has_out 'Log out and back in'

host docker up dbgroup enabled compose systemd
rm "$case_dir/bin/sg"
run -- --prepare-only
check 'no sg: sudo -u fallback' has_log 'sudo -u alice env RUNQUILL_INSTALL_REEXEC=1'
check 'no sg: ready' exit_is 0

host $healthy
run STUB_HTTP=000 -- --prepare-only
check 'offline: refused' has_out 'no HTTPS connection to github.com ghcr.io'

host $healthy
run STUB_DISK_KB=10485760 -- --prepare-only
check 'low disk: warning only' exit_is 0
check 'low disk: says so' has_out '10 GB free in /var/lib/docker; runner images'

host systemd k3s
run -- --prepare-only --yes
check 'k3s without docker: skipped' has_out 'K3s found, so the Docker checks are skipped'
check 'k3s without docker: no docker install' lacks_log 'docker-ce'

host systemd
run -- --prepare-only --no-docker
check '--no-docker: skipped' has_out 'skipped (--no-docker)'
check '--no-docker: ready' exit_is 0

host systemd
run -- --upgrade --version v0.3.1 --dir ./missing
check 'upgrade: skips Docker probes' lacks_log 'docker '
check 'upgrade: no docker install' changed_nothing
check 'upgrade: still requires an install' has_out 'No Runquill install found'

host
release
mkdir -p "$case_dir/elsewhere/-run quill's"
run CDPATH=../elsewhere -- --download-only --version v0.3.2 --dir "-run quill's"
installed="$case_dir/home/-run quill's"
check 'download: accepts a relative directory with shell punctuation' exit_is 0
check 'download: verified binary installed in the requested directory' cmp "$fixture/runquill-linux-amd64" "$installed/runquill"
check 'download: verifies and installs optional asset' cmp "$fixture/compose.panel.yaml" "$installed/compose.panel.yaml"
check 'download: ignores CDPATH' test ! -e "$case_dir/elsewhere/-run quill's/runquill"
check 'download: removes staging on success' no_matches "$installed"/.runquill-install.*

host
release
run -- --download-only --version "v0.3.2${newline}extra"
check 'download: rejects a multiline version' exit_is 1
check 'download: invalid version does not fetch assets' lacks_log '/SHA256SUMS'

host $healthy
release
mkdir "$case_dir/home/temp"
printf '%s\n' 'corrupt optional asset' >"$fixture/compose.panel.yaml"
run TMPDIR=temp -- --wizard --yes --version v0.3.2
check 'download: rejects an optional asset with the wrong checksum' exit_is 1
check 'download: checksum error identifies the asset' has_out 'compose.panel.yaml: FAILED'
check 'download: no binary installed after checksum failure' test ! -e "$case_dir/home/runquill/runquill"
check 'download: removes staging on checksum failure' no_matches "$case_dir/home/runquill"/.runquill-install.*
check 'download: removes relative Tailscale temp directory after cd' no_matches "$case_dir/home/temp"/runquill-tailscale.*

host
release
sed '/ LICENSE$/d' "$fixture/SHA256SUMS" >"$fixture/manifest"
mv "$fixture/manifest" "$fixture/SHA256SUMS"
run -- --download-only --version v0.3.2
check 'download: rejects a missing required checksum' exit_is 1
check 'download: missing checksum identifies the asset' has_out 'Invalid or missing checksum for LICENSE'
check 'download: no binary installed with an incomplete manifest' test ! -e "$case_dir/home/runquill/runquill"

host
release
existing_install
run -- --upgrade --version v0.3.2
check 'upgrade: completes without Docker' exit_is 0
check 'upgrade: replaces the binary' cmp "$fixture/runquill-linux-amd64" "$installed/runquill"
check 'upgrade: backs up the replaced binary' grep -qx 'old binary' "$installed/.previous/runquill"
check 'upgrade: backs up the replaced Compose file' grep -qx 'old compose' "$installed/.previous/compose.yaml"
check 'upgrade: preserves .env' cmp "$case_dir/original.env" "$installed/.env"
run -- --upgrade --version v0.3.2
check 'upgrade: retry reports the release is already installed' has_out 'is already installed'
check 'upgrade: retry keeps the rollback copy' grep -qx 'old binary' "$installed/.previous/runquill"

images='RUNQUILL_MANAGER_IMAGE=ghcr.io/o/runquill:v0.3.1'
host
release
existing_install "$images" 'COMPOSE_FILE=compose.yaml:compose.panel.yaml'
run -- --upgrade --version v0.3.2
check 'upgrade hint: recorded Compose files need no -f' has_out 'listed in COMPOSE_FILE in .env'
check 'upgrade hint: recorded Compose files use plain commands' has_out '  docker compose up -d --no-build'
check 'upgrade hint: Compose install gets no K3s line' lacks_out 'kubectl'

host
release
existing_install "$images"
run -- --upgrade --version v0.3.2
check 'upgrade hint: older install is reminded of overlays' has_out 'add -f compose.yaml -f compose.panel.yaml'
check 'upgrade hint: older install still sees the K3s line' has_out 'On K3s, apply the new manifest instead'

host
release
existing_install "$images" 'RUNQUILL_BACKEND=kubernetes'
run -- --upgrade --version v0.3.2
check 'upgrade hint: K3s install applies the manifest' has_out 'kubectl apply -f runquill-k3s.yaml'
check 'upgrade hint: K3s install gets no Compose commands' lacks_out 'docker compose'

host
release
existing_install
cp "$work/templates/mv" "$case_dir/bin/"
run -- --upgrade --version v0.3.2
check 'upgrade: backup publish failure aborts' exit_is 1
check 'upgrade: backup publish failure restores the prior backup' grep -qx 'older binary' "$installed/.previous/runquill"
check 'upgrade: backup publish failure leaves current files intact' grep -qx 'old binary' "$installed/runquill"
check 'upgrade: backup publish failure preserves .env' cmp "$case_dir/original.env" "$installed/.env"
check 'upgrade: backup publish failure removes staging' no_matches "$installed"/.runquill-install.*
check 'upgrade: backup publish failure releases the lock' test ! -e "$installed/.runquill-upgrade"

host
release
existing_install
cp "$work/templates/mkdir" "$case_dir/bin/"
run -- --upgrade --version v0.3.2
check 'upgrade: interruption while locking exits' exit_is 130
check 'upgrade: interruption while locking removes staging' no_matches "$installed"/.runquill-install.*
check 'upgrade: interruption while locking releases the lock' test ! -e "$installed/.runquill-upgrade"

script_bin=$(command -v script 2>/dev/null || true)
if [ -n "$script_bin" ] && "$script_bin" -qec true /dev/null >/dev/null 2>&1; then
    host systemd
    run_tty y --prepare-only
    check 'TTY yes: asked' has_out 'Make these changes? [y/N]'
    check 'TTY yes: installed' has_log 'docker-ce'
    check 'TTY yes: ready' has_out 'Host is ready.'

    host systemd
    run_tty n --prepare-only
    check 'TTY no: refused' has_out 'Nothing was changed. Make the changes above yourself'
    check 'TTY no: nothing changed' changed_nothing

    host $healthy tailscale
    STUB_TAILSCALE_STATE=NeedsLogin run_tty y --wizard --version v0.3.2 --yes
    check 'TTY wizard displays Tailscale sign-in instructions' has_out 'Open the Tailscale sign-in link'
    check 'TTY wizard starts a bounded login' has_log 'tailscale up --timeout=2m'

    host $healthy tailscale
    STUB_STREAMED=1 run_tty y --wizard --version v0.3.2
    check 'streamed installer reads consent from the controlling terminal' has_log 'tailscale set --operator=alice'
else
    printf 'skip: TTY cases (util-linux script not available)\n'
fi

printf '%s: %s passed, %s failed\n' "$shell_under_test" "$passed" "$failed"
[ "$failed" -eq 0 ]
