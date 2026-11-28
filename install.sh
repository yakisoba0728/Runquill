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
assume_yes=false
no_install=false
prepare_only=false
no_docker=false
no_tailscale=false

# Docker's published signing keys (https://docs.docker.com/engine/install/).
docker_apt_fingerprint=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
docker_rpm_fingerprint=060A61C51B558A7F742B77AAC52FEB6B621E9F35
docker_packages='docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin'
# Only the offline installer tests set this; it prefixes /etc, /proc, /run and /sys reads.
sysroot=${RUNQUILL_INSTALL_SYSROOT:-}

usage() {
    cat <<'EOF_USAGE'
Usage: sh install.sh [--dir PATH] [--version vX.Y.Z] [--wizard] [--yes | --no-install]
                     [--no-docker] [--no-tailscale]
       sh install.sh [--dir PATH] [--version vX.Y.Z] [--webhook-url URL] [--listen-port PORT]
                     [--repository-scope all|selected] [--organization ORG [--runner-group-id ID]]
                     [--panel [--panel-url URL]] [--yes | --no-install]
       sh install.sh --upgrade [--version vX.Y.Z] [--dir PATH] [--yes | --no-install]
       sh install.sh --prepare-only [--yes | --no-install] [--no-docker]
       sh install.sh --download-only [--version vX.Y.Z] [--dir PATH]

The installer checks Linux, cgroup v2 and Docker, then offers to prepare the
host. The browser setup can also install and connect Tailscale for webhooks.

  --dir PATH                 Install directory (default: ./runquill).
  --version vX.Y.Z           Release to install (default: latest release).
  --wizard                   Download, then finish setup in the browser wizard.
  -y, --yes                  Apply the listed host changes without asking.
  --no-install               Skip system changes; fail if a requirement is missing.
  --no-docker                Skip the Docker checks (K3s hosts, or Docker managed
                             elsewhere).
  --no-tailscale             Skip managed Tailscale setup in the wizard.
  --prepare-only             Check and prepare the host, then stop.
  --download-only            Download and verify the release only; no host checks,
                             setup or Compose.
  --webhook-url URL          Subscribe the new GitHub App to webhooks at URL.
  --listen-port PORT         Local port for the setup callback or the wizard.
  --repository-scope SCOPE   Install the App on all or selected repositories.
  --organization ORG         Create the GitHub App under organization ORG.
  --runner-group-id ID       Register runners in this organization runner group.
  --panel                    Configure the web panel and start Compose with it.
  --panel-url URL            Browser origin of the panel (requires --panel).
  --upgrade                  Replace the release files of an existing install in
                             --dir after verifying checksums. Replaced files are
                             kept in .previous/. .env, keys and data are untouched.
  -h, --help                 Show this help.

The installer asks once before changing the host. Tailscale sign-in requires
a terminal and account approval. Polling works without Tailscale.
EOF_USAGE
}

fail() {
    printf 'runquill install: %s\n' "$*" >&2
    exit 1
}

require() {
    command -v "$1" >/dev/null 2>&1 || fail "$1 is required but was not found. $2"
}

# quote prints $1 as one single-quoted shell word.
quote() {
    # Keep trailing newlines through command substitution.
    quoted=$(printf '%s.\n' "$1" | sed "s/'/'\\\\''/g")
    printf "'%s'" "${quoted%.}"
}

original_args=
for argument in "$@"; do original_args="$original_args $(quote "$argument")"; done
case $0 in
    /*) script_path=$0 ;;
    *) script_path=$(pwd)/$0 ;;
esac

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
        -y|--yes) assume_yes=true; shift ;;
        --no-install) no_install=true; shift ;;
        --prepare-only) prepare_only=true; shift ;;
        --no-docker) no_docker=true; shift ;;
        --no-tailscale) no_tailscale=true; shift ;;
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
if [ -n "$listen_port" ]; then
    case $listen_port in
        *[!0-9]*) printf '%s\n' '--listen-port takes a port number.' >&2; exit 2 ;;
    esac
fi

if [ "$wizard" = true ] && { [ "$panel" = true ] || [ -n "$webhook$repository_scope$organization$runner_group_id" ]; }; then
    printf '%s\n' 'Set GitHub, panel, and webhook options inside the wizard.' >&2
    exit 2
fi

if [ "$upgrade" = true ] && { [ "$wizard" = true ] || [ "$panel" = true ] || [ -n "$webhook$listen_port$panel_url$repository_scope$organization$runner_group_id" ]; }; then
    printf '%s\n' '--upgrade only accepts --version, --dir, --yes and --no-install; it never reruns setup.' >&2
    exit 2
fi
if [ "$assume_yes" = true ] && [ "$no_install" = true ]; then
    printf '%s\n' '--yes and --no-install contradict each other. Pick one.' >&2
    exit 2
fi
if [ "$prepare_only" = true ] && { [ "$download_only" = true ] || [ "$upgrade" = true ]; }; then
    printf '%s\n' '--prepare-only cannot be combined with --download-only or --upgrade.' >&2
    exit 2
fi
if [ "$download_only" = true ] && [ "$upgrade" = true ]; then
    printf '%s\n' '--download-only cannot be combined with --upgrade.' >&2
    exit 2
fi

# item STATUS NAME DETAIL prints one aligned checklist line.
item() {
    if [ "$1" = ok ] && [ "$prepare_only" = false ]; then return 0; fi
    printf '  %-4s  %-14s %s\n' "$1" "$2" "$3"
}

# ---------------------------------------------------------------- host facts

has() { command -v "$1" >/dev/null 2>&1; }

os_value() {
    if [ -r "$sysroot/etc/os-release" ]; then
        awk -F= -v key="$1" '$1 == key {
            value = substr($0, index($0, "=") + 1)
            gsub(/^["'\'']|["'\'']$/, "", value)
            print value
            exit
        }' "$sysroot/etc/os-release"
    fi
}

in_group_list() { # LIST GROUP
    case " $1 " in *" $2 "*) return 0 ;; esac
    return 1
}

have_tty() {
    [ -t 0 ] && return 0
    (: </dev/tty) 2>/dev/null
}

uid=$(id -u)
user=$(id -un)

# https_status URL prints the HTTP status code, or 000 when HTTPS fails.
https_status() {
    https_code=$(curl --proto '=https' --proto-redir '=https' -sS -o /dev/null -I \
        --connect-timeout 10 --max-time 20 -w '%{http_code}' "$1" 2>/dev/null) || true
    printf '%s\n' "${https_code:-000}"
}

# docker_repo_state sets docker_repo (apt/dnf/apk/none), docker_repo_url,
# docker_repo_note and docker_repo_reason for this distribution.
docker_repo_state() {
    docker_repo=none docker_repo_url='' docker_repo_note='' docker_repo_reason=''
    case $os_id in
        ubuntu|debian)
            if ! has apt-get; then docker_repo_reason="apt-get not found on $os_name"; return; fi
            docker_repo=apt
            docker_repo_dist=$os_id
            if [ "$os_id" = ubuntu ]; then
                docker_repo_suite=$(os_value UBUNTU_CODENAME)
                [ -n "$docker_repo_suite" ] || docker_repo_suite=$(os_value VERSION_CODENAME)
            else
                docker_repo_suite=$(os_value VERSION_CODENAME)
            fi
            if [ -z "$docker_repo_suite" ]; then
                docker_repo=none docker_repo_reason="$os_name has no release codename in /etc/os-release"
                return
            fi
            docker_repo_url=https://download.docker.com/linux/$docker_repo_dist
            docker_repo_probe=$docker_repo_url/dists/$docker_repo_suite/Release
            ;;
        fedora|rhel|centos|rocky|almalinux)
            if has dnf; then docker_dnf=dnf; elif has yum; then docker_dnf=yum; else
                docker_repo_reason="dnf/yum not found on $os_name"; return
            fi
            docker_repo=dnf
            case $os_id in
                fedora) docker_repo_dist=fedora docker_repo_suite=$os_version ;;
                rhel) docker_repo_dist=rhel docker_repo_suite=${os_version%%.*} ;;
                *) docker_repo_dist=centos docker_repo_suite=${os_version%%.*} ;;
            esac
            docker_repo_url=https://download.docker.com/linux/$docker_repo_dist
            docker_repo_probe=$docker_repo_url/$docker_repo_suite/
            ;;
        alpine)
            if has apk; then
                docker_repo=apk
                docker_repo_note="Docker's repository does not support Alpine, so Alpine's own community packages are used"
            else
                docker_repo_reason='apk not found'
            fi
            ;;
        *)
            docker_repo_reason="automatic Docker install is not available for ${os_name:-this distribution}"
            ;;
    esac
}

# docker_repo_available checks that Docker's repository has this release.
docker_repo_available() {
    case $docker_repo in
        apt|dnf) [ "$(https_status "$docker_repo_probe")" = 200 ] ;;
        *) return 0 ;;
    esac
}

docker_repo_configured() {
    case $docker_repo in
        apt) grep -rqs 'download\.docker\.com' "$sysroot/etc/apt/sources.list" "$sysroot/etc/apt/sources.list.d" ;;
        dnf) grep -rqs 'download\.docker\.com' "$sysroot/etc/yum.repos.d" ;;
        *) return 1 ;;
    esac
}

docker_from_docker_repo() {
    if has dpkg-query; then
        dpkg-query -W -f='${Status}' docker-ce 2>/dev/null | grep -q 'install ok installed'
    elif has rpm; then
        rpm -q docker-ce >/dev/null 2>&1
    else
        return 1
    fi
}

tool_package() { # TOOL prints the package that provides it for $pkg_manager
    case $1 in
        curl) case $pkg_manager in apt-get) printf 'curl ca-certificates' ;; *) printf curl ;; esac ;;
        sha256sum|mktemp|df|stat|nproc) printf coreutils ;;
        awk) printf gawk ;;
        cmp) printf diffutils ;;
        gpg) case $pkg_manager in apt-get) printf gpg ;; dnf|yum) printf gnupg2 ;; zypper) printf gpg2 ;; *) printf gnupg ;; esac ;;
        *) printf '%s' "$1" ;;
    esac
}

# ---------------------------------------------------------------- preflight

add_line() { # VAR TEXT appends one line to a newline-separated list
    eval "add_line_old=\${$1}"
    if [ -n "$add_line_old" ]; then
        eval "$1=\${$1}\$newline\$2"
    else
        eval "$1=\$2"
    fi
}
newline='
'

as_root_text() {
    if [ "$uid" -eq 0 ]; then printf '%s' "$*"; else printf 'sudo %s' "$*"; fi
}

plan_cmd() { add_line plan "    $(as_root_text "$@")"; }

preflight() {
    plan='' blockers='' manual='' docker_plan=''
    fix_tools='' fix_docker=false fix_compose=false fix_start=false fix_enable=false
    fix_group=false activate_group=false docker_root=''
    mode_docker=true
    if [ "$no_docker" = true ] || [ "$upgrade" = true ]; then mode_docker=false; fi

    printf 'Checking this host\n'

    # System and architecture.
    kernel=$(uname -s)
    machine=$(uname -m)
    os_id=$(os_value ID)
    os_version=$(os_value VERSION_ID)
    os_name=$(os_value PRETTY_NAME)
    [ -n "$os_name" ] || os_name=${os_id:-unknown Linux}
    case $machine in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) arch= ;; esac
    if [ "$kernel" != Linux ]; then
        item fail System "$kernel $machine is not supported"
        add_line blockers "Runquill runs on Linux amd64 or arm64 servers. Run the installer on that server, not on $kernel."
        return
    fi
    if [ -z "$arch" ]; then
        item fail System "$os_name, Linux $machine"
        add_line blockers "Runquill releases exist only for Linux amd64 (x86_64) and arm64 (aarch64), not $machine."
        return
    fi
    item ok System "$os_name, Linux $arch"

    if has apt-get; then pkg_manager=apt-get
    elif has dnf; then pkg_manager=dnf
    elif has yum; then pkg_manager=yum
    elif has zypper; then pkg_manager=zypper
    elif has apk; then pkg_manager=apk
    else pkg_manager=
    fi

    can_root=true root_reason=''
    if [ "$uid" -ne 0 ]; then
        if ! has sudo; then
            can_root=false root_reason='this needs root, and sudo is not installed. Rerun as root, or install sudo and rerun.'
        elif ! sudo -n true 2>/dev/null && ! have_tty; then
            can_root=false root_reason='sudo needs a password and there is no terminal to ask for it. Run the installer from an interactive shell, or allow passwordless sudo.'
        fi
    fi

    # cgroup v2.
    cgroup_fs=$(stat -fc %T "$sysroot/sys/fs/cgroup" 2>/dev/null || true)
    # BusyBox stat prints UNKNOWN for cgroup2; only a cgroup v2 root has cgroup.controllers.
    if [ -f "$sysroot/sys/fs/cgroup/cgroup.controllers" ]; then cgroup_fs=cgroup2fs; fi
    if [ "$cgroup_fs" = cgroup2fs ]; then
        item ok cgroup v2
    elif [ "$upgrade" = true ]; then
        item warn cgroup "v1 or unknown ($cgroup_fs); Runquill runners need cgroup v2"
    else
        item fail cgroup "v1 or unknown (${cgroup_fs:-not mounted})"
        add_line blockers 'Runquill needs cgroup v2. Boot with systemd.unified_cgroup_hierarchy=1 (or upgrade to a distribution release that uses cgroup v2), then rerun.'
    fi

    # Tools the installer itself uses.
    missing_tools=
    for tool in curl sha256sum awk grep sed cmp mktemp df; do
        has "$tool" || missing_tools="$missing_tools $tool"
    done

    # Docker.
    if [ "$mode_docker" = true ]; then
        docker_present=false
        if has docker; then docker_present=true; fi
        if [ "$docker_present" = false ] && has k3s; then
            mode_docker=false
            item skip Docker 'K3s found, so the Docker checks are skipped'
        else
            docker_repo_state
            preflight_docker
        fi
    elif [ "$no_docker" = true ]; then
        item skip Docker 'skipped (--no-docker)'
    fi
    if [ "$fix_docker" = true ] && [ "$docker_repo" != apk ] && ! has gpg; then
        missing_tools="$missing_tools gpg"
    fi

    if [ -n "$missing_tools" ]; then
        tool_packages=
        for tool in $missing_tools; do
            for package in $(tool_package "$tool"); do
                in_group_list "$tool_packages" "$package" || tool_packages="$tool_packages $package"
            done
        done
        tool_packages=${tool_packages# }
        if [ -z "$pkg_manager" ]; then
            item fail Tools "missing:$missing_tools"
            add_line blockers "Install these tools with your package manager, then rerun:$missing_tools (packages: $tool_packages)."
        elif [ "$can_root" = false ]; then
            item fail Tools "missing:$missing_tools"
            add_line blockers "Installing$missing_tools: $root_reason"
        else
            item fix Tools "missing:$missing_tools; install $tool_packages with $pkg_manager"
            fix_tools=$tool_packages
            add_line plan "  Install$missing_tools:"
            case $pkg_manager in
                apt-get)
                    plan_cmd env DEBIAN_FRONTEND=noninteractive apt-get update
                    plan_cmd env DEBIAN_FRONTEND=noninteractive apt-get install -y "$fix_tools" ;;
                apk) plan_cmd apk add --no-cache "$fix_tools" ;;
                zypper) plan_cmd zypper --non-interactive install "$fix_tools" ;;
                *) plan_cmd "$pkg_manager" install -y "$fix_tools" ;;
            esac
        fi
    else
        item ok Tools 'curl sha256sum awk grep sed cmp mktemp df'
    fi
    if [ -n "$docker_plan" ]; then add_line plan "$docker_plan"; fi

    # Disk space where Docker keeps images and containers.
    if [ "$mode_docker" = true ]; then
        disk_dir=$docker_root
        if [ -z "$disk_dir" ]; then
            for disk_dir in /var/lib/docker /var/lib /; do [ -d "$disk_dir" ] && break; done
        fi
        disk_gb=$(df -Pk "$disk_dir" 2>/dev/null | awk 'NR == 2 { print int($4 / 1048576) }') || true
        if [ -n "$disk_gb" ] && [ "$disk_gb" -lt 20 ]; then
            item warn Disk "$disk_gb GB free in $disk_dir; runner images and job workspaces need about 20 GB"
        fi
    fi

    # An explicit preflight checks connectivity. Installation uses the real
    # download and image pull, which also report their failures.
    if [ "$prepare_only" = false ]; then
        return 0
    elif ! has curl; then
        item wait Network 'checked after curl is installed'
    else
        unreachable=
        for host in github.com ghcr.io; do
            case $host in ghcr.io) probe=https://ghcr.io/v2/ ;; *) probe=https://$host/ ;; esac
            [ "$(https_status "$probe")" != 000 ] || unreachable="$unreachable $host"
        done
        if [ -z "$unreachable" ]; then
            item ok Network 'HTTPS to github.com and ghcr.io'
        else
            item fail Network "no HTTPS connection to$unreachable"
            add_line blockers "Allow outbound HTTPS (port 443) to$unreachable. Behind a proxy, export HTTPS_PROXY before running the installer."
        fi
    fi
}

preflight_docker() {
    docker_plan=
    group_now=$(id -Gn)
    group_db=$(id -Gn "$user" 2>/dev/null || printf '%s' "$group_now")
    systemd=false
    if [ -d "$sysroot/run/systemd/system" ] && has systemctl; then systemd=true; fi
    openrc=false
    if [ "$systemd" = false ] && [ -e "$sysroot/run/openrc/softlevel" ] && has rc-service; then openrc=true; fi

    snap=false
    if [ "$docker_present" = true ]; then
        docker_path=$(command -v docker)
        case $docker_path in /snap/*) snap=true ;; esac
        case $(readlink -f "$docker_path" 2>/dev/null || true) in /snap/*|*/snap/*) snap=true ;; esac
    fi
    rootless_host=false
    case ${DOCKER_HOST:-} in *"/run/user/"*|*"/.docker/run/"*) rootless_host=true ;; esac

    # Engine.
    if [ "$docker_present" = false ]; then
        if [ "$docker_repo" = none ]; then
            item fail 'Docker Engine' "not installed; $docker_repo_reason"
            add_line blockers "Install Docker Engine by following https://docs.docker.com/engine/install/ , then rerun."
        elif [ "$can_root" = false ]; then
            item fail 'Docker Engine' 'not installed'
            add_line blockers "Installing Docker Engine: $root_reason"
        elif [ "$prepare_only" = true ] && has curl && ! docker_repo_available; then
            item fail 'Docker Engine' "not installed; Docker's repository has no packages for $os_name"
            add_line blockers "Docker's repository does not offer $os_name ($docker_repo_probe). Follow https://docs.docker.com/engine/install/ , then rerun."
        else
            fix_docker=true
            case $docker_repo in
                apt)
                    item fix 'Docker Engine' "not installed; install from Docker's apt repository ($docker_repo_suite)"
                    docker_plan="  Install Docker Engine and Compose from $docker_repo_url:"
                    if ! docker_repo_configured; then
                        docker_plan="$docker_plan$newline    Verify Docker's signing key ($docker_apt_fingerprint) and add the apt repository at /etc/apt/sources.list.d/docker.sources."
                    fi
                    docker_plan="$docker_plan$newline    $(as_root_text env DEBIAN_FRONTEND=noninteractive apt-get install -y "$docker_packages")"
                    ;;
                dnf)
                    item fix 'Docker Engine' "not installed; install from Docker's $docker_repo_dist repository"
                    docker_plan="  Install Docker Engine and Compose from $docker_repo_url:"
                    if ! docker_repo_configured; then
                        docker_plan="$docker_plan$newline    Verify Docker's signing key ($docker_rpm_fingerprint) and add the RPM repository at /etc/yum.repos.d/docker-ce.repo."
                    fi
                    docker_plan="$docker_plan$newline    $(as_root_text "$docker_dnf" install -y "$docker_packages")"
                    ;;
                apk)
                    item fix 'Docker Engine' "not installed; install Alpine's docker packages"
                    docker_plan="  Install Docker Engine and Compose ($docker_repo_note):"
                    docker_plan="$docker_plan$newline    $(as_root_text apk add --no-cache docker docker-cli-compose)"
                    ;;
            esac
        fi
    elif [ "$snap" = true ]; then
        item warn 'Docker Engine' "snap package ($docker_path)"
    else
        item ok 'Docker Engine' 'installed'
    fi

    # Compose v2.
    if [ "$fix_docker" = true ]; then
        item fix 'Compose v2' 'installed with Docker Engine'
    elif [ "$docker_present" = false ]; then
        item fail 'Compose v2' 'needs Docker Engine'
    elif compose_version=$(docker compose version --short 2>/dev/null); then
        item ok 'Compose v2' "$compose_version"
    elif [ "$snap" = true ]; then
        item fail 'Compose v2' 'missing from the snap Docker'
        add_line blockers 'Snap-installed Docker is not changed automatically. Remove it (sudo snap remove docker) and rerun to install Docker from its official repository, or add the Compose v2 plugin yourself.'
    elif [ "$can_root" = false ]; then
        item fail 'Compose v2' 'missing'
        add_line blockers "Installing Docker Compose v2: $root_reason"
    elif docker_from_docker_repo && { [ "$docker_repo" = apt ] || [ "$docker_repo" = dnf ]; }; then
        fix_compose=true
        item fix 'Compose v2' 'missing; install docker-compose-plugin'
        if [ "$docker_repo" = apt ]; then
            add_line docker_plan "  Install the Compose plugin:$newline    $(as_root_text env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-compose-plugin)"
        else
            add_line docker_plan "  Install the Compose plugin:$newline    $(as_root_text "$docker_dnf" install -y docker-compose-plugin)"
        fi
    elif [ "$docker_repo" = apk ]; then
        fix_compose=true
        item fix 'Compose v2' 'missing; install docker-cli-compose'
        add_line docker_plan "  Install the Compose plugin:$newline    $(as_root_text apk add --no-cache docker-cli-compose)"
    else
        item fail 'Compose v2' 'missing'
        add_line blockers 'Docker Compose v2 is missing and this Docker was not installed from Docker'\''s repository, so it is not changed automatically. Install the Compose plugin: https://docs.docker.com/compose/install/linux/'
    fi

    # Daemon and access.
    daemon=unknown
    docker_info=
    if [ "$docker_present" = true ]; then
        if docker_info=$(docker info --format '{{json .SecurityOptions}}' 2>&1); then
            daemon=reachable
        elif printf '%s' "$docker_info" | grep -qi 'permission denied'; then
            daemon=denied
        else
            daemon=down
        fi
    fi
    if [ "$daemon" = reachable ]; then
        docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)
        if printf '%s' "$docker_info" | grep -q rootless || [ "$rootless_host" = true ]; then
            item fail 'Docker daemon' 'rootless'
            add_line blockers "This Docker daemon runs rootless, but Runquill's default runners are privileged containers that need rootful Docker. The installer does not change it. Use the system daemon instead: unset DOCKER_HOST, run 'docker context use default', and make sure the rootful docker service is installed and running (https://docs.docker.com/engine/install/). Or route jobs to a pool with privileged: false."
            return
        fi
        item ok 'Docker daemon' "running, data in ${docker_root:-/var/lib/docker}"
    elif [ "$rootless_host" = true ]; then
        item fail 'Docker daemon' "not reachable at DOCKER_HOST=${DOCKER_HOST:-}"
        add_line blockers "DOCKER_HOST points at a rootless daemon. Runquill needs the rootful system daemon: unset DOCKER_HOST and rerun."
        return
    elif [ "$daemon" = denied ]; then
        item ok 'Docker daemon' 'running (socket exists)'
    elif [ "$daemon" = down ] || [ "$fix_docker" = true ]; then
        if [ "$snap" = true ]; then
            item fail 'Docker daemon' 'not running (snap)'
            add_line blockers 'Start the snap Docker yourself (sudo snap start docker) and rerun; snap-installed Docker is not changed automatically.'
        elif [ "$systemd" = true ] && [ "$can_root" = true ]; then
            fix_start=true
            item fix 'Docker daemon' 'start and enable at boot with systemctl'
            add_line docker_plan "  Start Docker now and at every boot:$newline    $(as_root_text systemctl enable --now docker)"
        elif [ "$openrc" = true ] && [ "$can_root" = true ]; then
            fix_start=true
            item fix 'Docker daemon' 'start and enable at boot with OpenRC'
            add_line docker_plan "  Start Docker now and at every boot:$newline    $(as_root_text rc-update add docker default)$newline    $(as_root_text rc-service docker start)"
        elif [ "$systemd" = false ] && [ "$openrc" = false ]; then
            item warn 'Docker daemon' 'not running; this host has no systemd, so it is not started automatically'
            add_line manual "Start the Docker daemon with this host's init system (or run 'sudo dockerd &' for a quick test), make it start at boot, then rerun the installer."
        else
            item fail 'Docker daemon' 'not running'
            add_line blockers "Starting Docker: $root_reason"
        fi
    fi
    if [ "$fix_start" = false ] && [ "$systemd" = true ] && [ "$daemon" != unknown ] && [ "$snap" = false ]; then
        if systemctl is-enabled docker.service >/dev/null 2>&1 || systemctl is-enabled docker.socket >/dev/null 2>&1; then
            item ok 'Start at boot' 'docker.service enabled'
        elif [ "$can_root" = true ]; then
            fix_enable=true
            item fix 'Start at boot' 'enable docker.service'
            add_line docker_plan "  Start Docker at every boot:$newline    $(as_root_text systemctl enable docker)"
        else
            item warn 'Start at boot' "docker.service is not enabled ($root_reason)"
        fi
    elif [ "$fix_start" = false ] && [ "$systemd" = false ] && [ "$daemon" = reachable ]; then
        item warn 'Start at boot' 'no systemd; make sure your init system starts dockerd at boot'
    fi

    if [ "$uid" -eq 0 ]; then
        item ok 'Docker access' 'running as root'
    elif [ "$daemon" = reachable ]; then
        item ok 'Docker access' "$user can use the daemon"
    elif in_group_list "$group_now" docker; then
        if [ "$daemon" = denied ]; then
            item fail 'Docker access' "$user is in the docker group but the socket refuses access"
            add_line blockers "Check the owner and mode of /var/run/docker.sock (expected root:docker, 660): $docker_info"
        else
            item ok 'Docker access' "$user is in the docker group"
        fi
    elif in_group_list "$group_db" docker; then
        activate_group=true
        item ok 'Docker access' "$user is in the docker group; this run continues with it active"
    elif [ "$snap" = true ]; then
        item fail 'Docker access' "$user cannot use the snap Docker"
        add_line blockers "Add $user to the docker group yourself or run the installer as root; snap-installed Docker is not changed automatically."
    elif [ "$can_root" = false ]; then
        item fail 'Docker access' "$user cannot use the daemon"
        add_line blockers "Adding $user to the docker group: $root_reason"
    else
        fix_group=true activate_group=true
        item fix 'Docker access' "add $user to the docker group (root-equivalent access)"
        if [ "$docker_repo" = apk ] || ! has usermod; then
            group_cmd="addgroup $user docker"
        else
            group_cmd="usermod -aG docker $user"
        fi
        add_line docker_plan "  Let $user use Docker without sudo. Members of the docker group can control the$newline  daemon and so have root-equivalent access to this host:$newline    $(as_root_text "$group_cmd")"
    fi
}

# ---------------------------------------------------------------- applying

applied=
apply_step=
prep_tmp='' ts_tmp='' scratch='' upgrade_lock=''
temp_root=${TMPDIR:-/tmp}
case $temp_root in /*) ;; *) temp_root=$(pwd)/$temp_root ;; esac
note_done() { add_line applied "  $1"; }

run_root() {
    if [ "$uid" -eq 0 ]; then
        printf '  + %s\n' "$*"
        "$@"
    else
        printf '  + sudo %s\n' "$*"
        sudo "$@"
    fi
}

on_exit() {
    exit_status=$?
    if [ -n "$prep_tmp" ]; then rm -rf -- "$prep_tmp" || :; fi
    if [ -n "$ts_tmp" ]; then rm -rf -- "$ts_tmp" || :; fi
    if [ -n "$scratch" ]; then
        if [ -d "$scratch/previous" ] && [ ! -e .previous ] && ! mv -- "$scratch/previous" .previous; then
            printf 'Could not restore .previous. The backup is in %s/%s/previous.\n' "$(pwd)" "$scratch" >&2
        else
            rm -rf -- "$scratch" || :
        fi
    fi
    if [ -n "$upgrade_lock" ]; then rmdir "$upgrade_lock" || :; fi
    if [ "$exit_status" -ne 0 ] && [ -n "$apply_step" ]; then
        printf '\nrunquill install: failed while: %s\n' "$apply_step" >&2
        if [ -n "$applied" ]; then printf 'Already changed on this host:\n%s\n' "$applied" >&2; else printf 'Nothing on this host was changed.\n' >&2; fi
        printf 'Not done: this step and everything after it. Fix the error above and rerun the installer;\nsteps that already succeeded are detected and skipped.\n' >&2
    fi
}
trap on_exit 0
trap 'exit 130' INT TERM

fetch_verified_key() { # URL FINGERPRINT OUT
    curl --proto '=https' --proto-redir '=https' -fsSL "$1" -o "$3"
    key_info=$(GNUPGHOME=$prep_tmp/gnupg gpg --batch --show-keys --with-colons "$3" 2>/dev/null) || key_info=
    key_pubs=$(printf '%s\n' "$key_info" | awk -F: '$1 == "pub" { n++ } END { print n + 0 }')
    key_fpr=$(printf '%s\n' "$key_info" | awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; exit }')
    if [ "$key_pubs" -ne 1 ] || [ "$key_fpr" != "$2" ]; then
        fail "The key from $1 has fingerprint '${key_fpr:-none}', not Docker's published $2. Nothing from it was trusted."
    fi
    printf '  verified key %s (fingerprint %s)\n' "$1" "$key_fpr"
}

# Package lists and group_cmd are split into words on purpose.
# shellcheck disable=SC2086
apply_plan() {
    prep_tmp=$(mktemp -d "$temp_root/runquill-prepare.XXXXXX")
    mkdir -m 700 "$prep_tmp/gnupg"
    if [ -n "$fix_tools" ]; then
        apply_step="install$missing_tools"
        case $pkg_manager in
            apt-get)
                run_root env DEBIAN_FRONTEND=noninteractive apt-get update
                run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y $fix_tools ;;
            apk) run_root apk add --no-cache $fix_tools ;;
            zypper) run_root zypper --non-interactive install $fix_tools ;;
            *) run_root "$pkg_manager" install -y $fix_tools ;;
        esac
        note_done "installed $fix_tools"
    fi
    if [ "$fix_docker" = true ]; then
        apply_step="install Docker Engine"
        case $docker_repo in
            apt)
                if ! docker_repo_configured; then
                    docker_repo_available || fail "Docker's repository does not offer $os_name ($docker_repo_probe). Follow https://docs.docker.com/engine/install/"
                    fetch_verified_key "$docker_repo_url/gpg" "$docker_apt_fingerprint" "$prep_tmp/docker.asc"
                    printf 'Types: deb\nURIs: %s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
                        "$docker_repo_url" "$docker_repo_suite" "$(dpkg --print-architecture)" >"$prep_tmp/docker.sources"
                    run_root install -m 0755 -d /etc/apt/keyrings
                    run_root install -m 0644 "$prep_tmp/docker.asc" /etc/apt/keyrings/docker.asc
                    note_done 'added Docker'\''s signing key as /etc/apt/keyrings/docker.asc'
                    run_root install -m 0644 "$prep_tmp/docker.sources" /etc/apt/sources.list.d/docker.sources
                    note_done 'added Docker'\''s apt repository in /etc/apt/sources.list.d/docker.sources'
                fi
                run_root env DEBIAN_FRONTEND=noninteractive apt-get update
                run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y $docker_packages
                ;;
            dnf)
                if ! docker_repo_configured; then
                    docker_repo_available || fail "Docker's repository does not offer $os_name ($docker_repo_probe). Follow https://docs.docker.com/engine/install/"
                    fetch_verified_key "$docker_repo_url/gpg" "$docker_rpm_fingerprint" "$prep_tmp/docker.gpg"
                    curl --proto '=https' --proto-redir '=https' -fsSL "$docker_repo_url/docker-ce.repo" -o "$prep_tmp/docker-ce.repo"
                    run_root rpm --import "$prep_tmp/docker.gpg"
                    note_done 'imported Docker'\''s signing key into rpm'
                    run_root install -m 0644 "$prep_tmp/docker-ce.repo" /etc/yum.repos.d/docker-ce.repo
                    note_done 'added Docker'\''s repository in /etc/yum.repos.d/docker-ce.repo'
                fi
                run_root "$docker_dnf" install -y $docker_packages
                ;;
            apk)
                run_root apk add --no-cache docker docker-cli-compose
                ;;
        esac
        note_done 'installed Docker Engine and the Compose plugin'
    fi
    if [ "$fix_compose" = true ]; then
        apply_step='install the Compose plugin'
        case $docker_repo in
            apt) run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-compose-plugin ;;
            dnf) run_root "$docker_dnf" install -y docker-compose-plugin ;;
            apk) run_root apk add --no-cache docker-cli-compose ;;
        esac
        note_done 'installed the Compose plugin'
    fi
    if [ "$fix_start" = true ]; then
        apply_step='start the Docker daemon'
        if [ "$systemd" = true ]; then
            run_root systemctl enable --now docker
        else
            run_root rc-update add docker default
            run_root rc-service docker start
        fi
        note_done 'started Docker and enabled it at boot'
    elif [ "$fix_enable" = true ]; then
        apply_step='enable Docker at boot'
        run_root systemctl enable docker
        note_done 'enabled Docker at boot'
    fi
    if [ "$fix_group" = true ]; then
        apply_step="add $user to the docker group"
        if ! getent group docker >/dev/null 2>&1 && ! grep -q '^docker:' /etc/group; then
            run_root groupadd docker
        fi
        run_root $group_cmd
        note_done "added $user to the docker group"
        RUNQUILL_INSTALL_GROUP_ADDED=1
        export RUNQUILL_INSTALL_GROUP_ADDED
    fi
    apply_step=
}

# continue_with_docker_group re-runs this installer, with the same arguments,
# in a process that has the docker group, so no new login is needed.
# shellcheck disable=SC2120 # it sets its own positional parameters
continue_with_docker_group() {
    if [ -n "${RUNQUILL_INSTALL_REEXEC:-}" ] || [ ! -r "$script_path" ]; then return 0; fi
    RUNQUILL_INSTALL_REEXEC=1
    export RUNQUILL_INSTALL_REEXEC
    if has sg; then
        printf '%s\n' 'Continuing with the docker group active (sg docker) ...'
        trap - 0
        on_exit
        exec sg docker -c "exec /bin/sh $(quote "$script_path")$original_args"
    fi
    if [ "$uid" -ne 0 ] && has sudo; then
        printf '%s\n' "Continuing with the docker group active (sudo -u $user) ..."
        trap - 0
        on_exit
        eval "set -- $original_args"
        exec sudo -u "$user" env RUNQUILL_INSTALL_REEXEC=1 \
            RUNQUILL_INSTALL_GROUP_ADDED="${RUNQUILL_INSTALL_GROUP_ADDED:-}" \
            HOME="${HOME:-}" PATH="$PATH" TMPDIR="${TMPDIR:-/tmp}" \
            SSH_CONNECTION="${SSH_CONNECTION:-}" DOCKER_HOST="${DOCKER_HOST:-}" \
            /bin/sh "$script_path" "$@"
    fi
}

relogin_note() {
    if [ -n "${RUNQUILL_INSTALL_GROUP_ADDED:-}" ]; then
        printf '%s\n' "Note: $user was added to the docker group. Shells opened before now need a new login (or 'newgrp docker') before docker works without sudo."
    fi
}

tailscale_state() {
    tailscale status --json 2>/dev/null |
        sed -n 's/^[[:space:]]*"BackendState":[[:space:]]*"\([^"]*\)".*$/\1/p'
}

tailscale_operator() {
    prefs=$(tailscale debug prefs 2>/dev/null) || return 1
    printf '%s\n' "$prefs" | sed -n 's/^[[:space:]]*"OperatorUser":[[:space:]]*"\([^"]*\)".*$/\1/p'
}

plan_tailscale() {
    tailscale_plan=
    if [ "$wizard" = false ] || [ "$mode_docker" = false ] || [ "$prepare_only" = true ] ||
        [ "$no_tailscale" = true ] || [ "$no_install" = true ] || [ "$can_root" = false ] ||
        [ -n "${RUNQUILL_INSTALL_REEXEC:-}" ]; then
        return 0
    fi
    if has tailscale; then
        case $(tailscale_state) in
            Running)
                [ "$uid" -ne 0 ] || return 0
                operator=$(tailscale_operator) || return 0
                [ -z "$operator" ] || return 0
                tailscale_plan='Prepare the existing Tailscale connection for Serve and Funnel.'
                ;;
            NeedsLogin|Stopped)
                if have_tty; then
                    tailscale_plan='Connect Tailscale in your browser for Serve and Funnel.'
                fi
                ;;
            '')
                if [ "$systemd" = true ] || [ "$openrc" = true ]; then
                    tailscale_plan='Start the Tailscale service and offer browser sign-in.'
                fi
                ;;
        esac
    elif [ "$systemd" = true ] || [ "$openrc" = true ]; then
        case $pkg_manager in apt-get|dnf|yum|apk|zypper)
            tailscale_plan='Install Tailscale from tailscale.com, enable its service at boot, and offer browser sign-in.'
            ;;
        esac
    fi
}

prepare_tailscale() {
    if ! has tailscale; then
        ts_tmp=$(mktemp -d "$temp_root/runquill-tailscale.XXXXXX") || return 1
        curl --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 30 \
            -fsSL https://tailscale.com/install.sh -o "$ts_tmp/install.sh" || return 1
        run_root sh "$ts_tmp/install.sh" || return 1
    fi
    state=$(tailscale_state)
    if [ -z "$state" ]; then
        if [ "$systemd" = true ]; then
            run_root systemctl enable --now tailscaled || return 1
        elif [ "$openrc" = true ]; then
            run_root rc-update add tailscale default && run_root rc-service tailscale start || return 1
        fi
        state=$(tailscale_state)
    fi
    if [ "$uid" -ne 0 ]; then
        operator=$(tailscale_operator) || return 1
        if [ -z "$operator" ]; then
            run_root tailscale set "--operator=$user" || return 1
        elif [ "$operator" != "$user" ]; then
            return 1
        fi
    fi
    case $state in
        Running) return 0 ;;
        NeedsLogin|Stopped)
            if have_tty; then
                printf '%s\n' 'Open the Tailscale sign-in link when it appears. Setup continues after two minutes.'
                tailscale up --timeout=2m
            else
                item info Tailscale 'sign in with tailscale up before using Funnel'
            fi
            ;;
        *) return 1 ;;
    esac
}

prepare_host() {
    preflight
    if [ -n "$blockers" ]; then
        printf '\nThe installer cannot continue:\n' >&2
        printf '%s\n' "$blockers" | sed 's/^/  - /' >&2
        if [ -n "$plan" ]; then printf 'Nothing on this host was changed.\n' >&2; fi
        exit 1
    fi
    plan_tailscale
    if [ -n "$plan$tailscale_plan" ]; then
        if [ -n "$plan" ]; then printf '\nTo prepare this host the installer will run:\n%s\n' "$plan"; fi
        if [ -n "$tailscale_plan" ]; then
            printf '\nOptional Tailscale setup (--no-tailscale to skip):\n  %s\n' "$tailscale_plan"
            if [ "$uid" -ne 0 ]; then
                printf '  If the local Tailscale operator is unassigned, authorize %s.\n' "$user"
            fi
        fi
        consent=false
        if [ "$no_install" = true ]; then
            fail 'Nothing was changed (--no-install). Make the changes above yourself, or rerun without --no-install.'
        elif [ "$assume_yes" = true ]; then
            consent=true
        elif have_tty; then
            printf 'Make these changes? [y/N] '
            answer=
            if [ -t 0 ]; then
                read -r answer || answer=
            else
                read -r answer </dev/tty || answer=
            fi
            case $answer in y|Y|yes|YES|Yes) consent=true ;; esac
            if [ "$consent" = false ] && [ -n "$plan" ]; then
                fail 'Nothing was changed. Make the changes above yourself, or rerun and answer y.'
            fi
        elif [ -n "$plan" ]; then
            printf '\nNothing was changed: there is no terminal to ask for consent.\n' >&2
            printf 'To allow these changes, rerun:\n  sh %s%s --yes\n' "$(quote "$0")" "$original_args" >&2
            exit 1
        fi
        if [ "$consent" = true ]; then
            if [ -n "$plan" ]; then apply_plan; fi
            if [ -n "$tailscale_plan" ] && ! prepare_tailscale; then
                item warn Tailscale 'automatic setup did not finish. Runquill can find jobs by polling.'
            fi
        fi
    fi
    if [ "$activate_group" = true ]; then
        continue_with_docker_group
    fi
    if [ -n "$plan" ]; then
        printf '\nChecking again\n'
        preflight
        if [ -n "$blockers$plan" ]; then
            printf '\nThe host is still not ready:\n' >&2
            if [ -n "$blockers" ]; then printf '%s\n' "$blockers" | sed 's/^/  - /' >&2; fi
            if [ -n "$plan" ]; then printf '%s\n' "$plan" >&2; fi
            if [ "$activate_group" = true ]; then
                printf '  - Log out and back in (so the docker group applies), then rerun the installer.\n' >&2
            fi
            exit 1
        fi
    fi
    if [ -n "$manual" ]; then
        printf '\nOne more step is needed:\n' >&2
        printf '%s\n' "$manual" | sed 's/^/  - /' >&2
        exit 1
    fi
    if [ "$activate_group" = true ] && [ "$uid" -ne 0 ] && ! docker info >/dev/null 2>&1; then
        fail "$user still cannot use Docker in this session. Log out and back in (so the docker group applies), then rerun the installer."
    fi
    printf 'Host is ready.\n\n'
}

# ---------------------------------------------------------------- main

# Keep host changes and file writes in one complete shell command, even when
# the installer is streamed into sh.
main() {
if [ "$download_only" = true ]; then
    if [ "$(uname -s)" != Linux ]; then
        printf '%s\n' 'Run the installer on the Linux server that will run Runquill.' >&2
        exit 1
    fi
    case "$(uname -m)" in
        x86_64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) printf '%s\n' 'Runquill releases support Linux amd64 and arm64.' >&2; exit 1 ;;
    esac
else
    prepare_host
    if [ "$prepare_only" = true ]; then
        relogin_note
        exit 0
    fi
    if [ "$mode_docker" = false ] && [ "$upgrade" = false ] && [ "$wizard" = false ] &&
        ! docker compose version --short >/dev/null 2>&1; then
        fail 'Docker Compose v2 is required for command-line setup. Install the Compose plugin or use --wizard for K3s.'
    fi
fi

require curl 'Install curl with your package manager.'
require sha256sum 'Install GNU coreutils (or BusyBox) to verify checksums.'
for tool in awk cmp grep mktemp; do
    require "$tool" 'Install it with your package manager.'
done
if [ -z "$tag" ]; then
    latest=$(curl --proto '=https' --proto-redir '=https' -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
    tag=${latest##*/}
fi
if [ "${tag#*"$newline"}" != "$tag" ] || ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$'; then
    printf '%s\n' 'Could not select a Runquill release tag. Specify --version vX.Y.Z.' >&2
    exit 1
fi

case $directory in /*) ;; *) directory=./$directory ;; esac
if [ "$upgrade" = true ]; then
    if [ ! -f "$directory/runquill" ] || [ -L "$directory/runquill" ]; then
        fail "No Runquill install found in $directory. Pass the existing install with --dir."
    fi
fi

umask 077
mkdir -p -- "$directory"
cd "$directory"
scratch=$(mktemp -d ./.runquill-install.XXXXXX)
binary=runquill-linux-$arch
url="$repo/releases/download/$tag"
files='compose.yaml compose.ssh.yaml LICENSE'
curl --proto '=https' --proto-redir '=https' --connect-timeout 10 --retry 2 \
    -fsSL "$url/SHA256SUMS" -o "$scratch/SHA256SUMS"
for optional in compose.panel.yaml compose.worker.yaml compose.mirror.yaml runquill-k3s.yaml; do
    if awk -v name="$optional" '$2 == name {found=1} END {exit !found}' "$scratch/SHA256SUMS"; then
        files="$files $optional"
    fi
done
set --
for name in "$binary" $files; do
    set -- "$@" "$url/$name" -o "$scratch/$name"
done
curl --proto '=https' --proto-redir '=https' --connect-timeout 10 --retry 2 --fail-early -fsSL "$@"
(
    cd "$scratch"
    awk -v names="$binary $files" '
        $2 != "" { seen[$2]++; hashes[$2] = $1 }
        END {
            total = split(names, wanted, " ")
            for (i = 1; i <= total; i++) {
                name = wanted[i]
                if (seen[name] != 1 || length(hashes[name]) != 64 || hashes[name] !~ /^[0-9a-f]+$/) {
                    print "Invalid or missing checksum for " name > "/dev/stderr"
                    exit 1
                }
                print hashes[name] "  " name
            }
        }' SHA256SUMS > verify.sha256
    sha256sum -c verify.sha256
)
chmod 700 "$scratch/$binary"
if [ "$upgrade" = true ]; then
    # Only release assets are replaced; .env, keys, data and anything else stay as they are.
    # Give cleanup ownership before honoring a signal received during mkdir.
    lock_interrupted=false
    trap 'lock_interrupted=true' INT TERM
    if (trap '' INT TERM; mkdir .runquill-upgrade); then
        upgrade_lock=.runquill-upgrade
    fi
    trap 'exit 130' INT TERM
    [ "$lock_interrupted" = false ] || exit 130
    [ -n "$upgrade_lock" ] || fail 'Could not lock this install for upgrade (.runquill-upgrade). Check for another installer.'
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
    backup=$scratch/backup
    mkdir "$backup"
    for name in $changed; do
        if [ -e "$name" ]; then cp -p -- "$name" "$backup/$name"; fi
    done
    if [ -d .previous ]; then mv -- .previous "$scratch/previous"; fi
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
    compose_file=
    backend=
    if [ -f .env ]; then
        manager_image=$(awk -F= '$1 == "RUNQUILL_MANAGER_IMAGE" {print $2}' .env)
        runner_image=$(awk -F= '$1 == "RUNQUILL_RUNNER_IMAGE" {print $2}' .env)
        compose_file=$(awk -F= '$1 == "COMPOSE_FILE" {print $2}' .env)
        backend=$(awk -F= '$1 == "RUNQUILL_BACKEND" {print $2}' .env)
    fi
    # Setup records RUNQUILL_BACKEND=kubernetes for K3s and COMPOSE_FILE for Compose; older installs have neither.
    if [ "$backend" = kubernetes ]; then
        printf '%s\n' 'Apply the new manifest to K3s: kubectl apply -f runquill-k3s.yaml'
    elif [ -n "$manager_image" ] || [ -n "$runner_image" ]; then
        printf '%s\n' 'Point the images in .env at the new release:'
        if [ -n "$manager_image" ]; then printf '  RUNQUILL_MANAGER_IMAGE=%s:%s\n' "${manager_image%:*}" "$tag"; fi
        if [ -n "$runner_image" ]; then printf '  RUNQUILL_RUNNER_IMAGE=%s:%s\n' "${runner_image%:*}" "$tag"; fi
        if [ -n "$compose_file" ]; then
            printf '%s\n' 'Then restart Compose from this directory. It uses the Compose files listed in COMPOSE_FILE in .env:'
        else
            printf '%s\n' 'Then restart Compose from this directory (add -f compose.yaml -f compose.panel.yaml, or your other overlays, if you started it with them):'
        fi
        if [ -n "$runner_image" ]; then printf '  docker pull %s:%s\n' "${runner_image%:*}" "$tag"; fi
        printf '%s\n' '  docker compose pull manager' '  docker compose up -d --no-build' '  docker compose exec -T manager runquill doctor'
    fi
    if [ -z "$backend" ] && [ -z "$compose_file" ] && [ -f runquill-k3s.yaml ]; then
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
    relogin_note
    set -- wizard
    if [ -n "$listen_port" ]; then set -- "$@" --listen-port "$listen_port"; fi
    if [ "$no_tailscale" = true ]; then export RUNQUILL_SKIP_TAILSCALE=1; fi
    ./runquill "$@"
    return
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
# Managers from v0.3.1 fetch missing runner images in the background.
case ${tag%%-*} in v0.[012].*|v0.3.0) docker pull "$runner_image" ;; esac
docker compose "$@" up -d --no-build --pull missing
docker compose "$@" exec -T manager runquill doctor
printf 'First job guide: %s/blob/%s/README.md#verify-your-first-job\n' "$repo" "$tag"
relogin_note
}

main
