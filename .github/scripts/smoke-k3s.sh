#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${K3D:=k3d}"
: "${RUNQUILL_TEST_K8S_MANAGER_IMAGE:=runquill-manager:ci}"
: "${RUNQUILL_TEST_K8S_RUNNER_BASE:=runquill-runner:ci}"
scratch=$(mktemp -d)
cluster="runquill-ci-$(basename "$scratch" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"
manager=$RUNQUILL_TEST_K8S_MANAGER_IMAGE
fixture="runquill-kube-fixture:$cluster"
tester="runquill-kube-tester:$cluster"
export KUBECONFIG="$scratch/kubeconfig"

cleanup() {
    local result=$?
    if [ "$result" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
        kubectl --request-timeout=10s get pods -A -o wide || true
        kubectl --request-timeout=10s -n runquill-system logs job/runquill-runtime-test || true
        kubectl --request-timeout=10s -n runquill-system logs deployment/runquill || true
        kubectl --request-timeout=10s -n runquill-runners get events --sort-by=.lastTimestamp || true
    fi
    "$K3D" cluster delete "$cluster" || result=$((result == 0 ? 1 : result))
    docker image rm "$fixture" "$tester" >/dev/null 2>&1 || true
    rm -r -- "$scratch"
    exit "$result"
}
trap cleanup EXIT

cp .github/testdata/k3s/Dockerfile .github/testdata/k3s/run.sh "$scratch/"
CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOHOSTARCH) go test -c -o "$scratch/kubernetes.test" ./internal/app
chmod 0755 "$scratch/kubernetes.test"
docker build --build-arg "RUNNER_IMAGE=$RUNQUILL_TEST_K8S_RUNNER_BASE" --target runner -t "$fixture" "$scratch"
docker build --build-arg "MANAGER_IMAGE=$manager" --target tester -t "$tester" "$scratch"
"$K3D" cluster create "$cluster" --image rancher/k3s:v1.37.2-k3s1 \
    --api-port 127.0.0.1:random --no-lb --kubeconfig-update-default=false --kubeconfig-switch-context=false \
    --k3s-arg '--disable=traefik,metrics-server,servicelb@server:*' --timeout 90s
"$K3D" kubeconfig get "$cluster" > "$KUBECONFIG"
"$K3D" image import "$manager" "$fixture" "$tester" -c "$cluster"

sed -e "s#ghcr.io/yakisoba0728/runquill:[^[:space:]]*#$manager#" \
    -e "s#ghcr.io/yakisoba0728/runquill-runner:[^[:space:]]*#$fixture#" \
    deploy/k3s/runquill.yaml | kubectl apply -f -
openssl genrsa -out "$scratch/github-app.pem" 2048
kubectl -n runquill-system create secret generic runquill-github-key --from-file="$scratch/github-app.pem"
kubectl -n runquill-system create secret generic runquill-config \
    --from-literal=RUNQUILL_APP_ID=123 --from-literal=RUNQUILL_OWNER=owner \
    --from-literal=RUNQUILL_WEBHOOK_SECRET=ci-fixture --from-literal=RUNQUILL_POLL_INTERVAL=0
kubectl -n runquill-system rollout status deployment/runquill --timeout=120s
kubectl -n runquill-system exec deployment/runquill -- runquill status --json |
    jq -e '.backend == "kubernetes" and .hosts[0].name == "kubernetes" and .hosts[0].status == "UP"'

sed -e "s#runquill-kube-tester:ci#$tester#" -e "s#runquill-kube-fixture:ci#$fixture#" \
    .github/testdata/k3s/job.yaml | kubectl apply -f -
kubectl -n runquill-system wait --for=condition=complete job/runquill-runtime-test --timeout=180s
kubectl -n runquill-system logs job/runquill-runtime-test

# Use cluster privileges for cleanup checks outside the manager's create-only Secret RBAC.
for ((attempt=0; attempt<20; attempt++)); do
    resources=$(kubectl -n runquill-runners get pods,secrets -o name)
    if [ -z "$resources" ]; then
        printf '%s\n' 'All runner Pods and credential Secrets were removed.'
        exit 0
    fi
    sleep 1
done
printf 'Runner resources remain after cleanup:\n%s\n' "$resources" >&2
exit 1
