ARG RUNNER_IMAGE=runquill-runner:ci
ARG MANAGER_IMAGE=runquill-manager:ci

FROM ${RUNNER_IMAGE} AS runner
COPY --chmod=755 run.sh /home/runner/run.sh

FROM ${MANAGER_IMAGE} AS tester
COPY kubernetes.test /usr/local/bin/kubernetes.test
ENTRYPOINT ["/usr/local/bin/kubernetes.test"]
