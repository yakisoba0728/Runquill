FROM --platform=$BUILDPLATFORM golang:1.26.11-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY examples/runquill-first-job.yml ./examples/
COPY panelweb/ ./panelweb/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# Release builds name their registry so the runner image catalogue can offer the published OS variants.
ARG RELEASE_IMAGE=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.releaseImage=$RELEASE_IMAGE" -o /out/runquill .

FROM docker:29-cli
LABEL org.opencontainers.image.licenses="Apache-2.0"
RUN command -v ssh >/dev/null || apk add --no-cache openssh-client
COPY --from=build /out/runquill /usr/local/bin/runquill
COPY LICENSE /usr/share/licenses/runquill/LICENSE
ENTRYPOINT ["/usr/local/bin/runquill"]
