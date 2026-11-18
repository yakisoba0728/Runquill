#!/bin/sh
set -eu

repo=https://github.com/yakisoba0728/Runquill
tag=$1
case "$(uname -m)" in
    x86_64) binary=runquill-linux-amd64 ;;
    aarch64) binary=runquill-linux-arm64 ;;
    *) printf '%s\n' 'Unsupported installer test architecture.' >&2; exit 1 ;;
esac

scratch=$(mktemp -d)
trap 'rm -r -- "$scratch"' EXIT
cd "$scratch"
url="$repo/releases/download/$tag"
for asset in install.sh SHA256SUMS; do
    curl --proto '=https' --proto-redir '=https' --retry 4 --retry-all-errors -fsSL "$url/$asset" -o "$asset"
done
awk '$2 == "install.sh"' SHA256SUMS | sha256sum -c -
sh ./install.sh --version "$tag" --download-only --dir ./installed
test "$(./installed/runquill --version)" = "$tag"

for name in "$binary" compose.yaml compose.ssh.yaml compose.panel.yaml runquill-k3s.yaml LICENSE; do
    installed=$name
    if [ "$name" = "$binary" ]; then installed=runquill; fi
    awk -v name="$name" -v path="installed/$installed" '$2 == name {print $1 "  " path}' SHA256SUMS |
        sha256sum -c -
done
grep -Fq "image: ghcr.io/yakisoba0728/runquill:$tag" installed/runquill-k3s.yaml
grep -Fq "value: ghcr.io/yakisoba0728/runquill-runner:$tag" installed/runquill-k3s.yaml

if awk '$2 == "compose.worker.yaml" {found=1} END {exit !found}' SHA256SUMS; then
    awk '$2 == "compose.worker.yaml" {print $1 "  installed/" $2}' SHA256SUMS | sha256sum -c -
fi
