FROM --platform=$BUILDPLATFORM golang:1.26.11-bookworm@sha256:ea47ee45f1284d60f9cc09cb2c3bb4f508c256b640bd8bb30342ff13d1adcb52 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal/ ./internal/
COPY examples/embed.go examples/runquill-first-job.yml ./examples/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# Release builds name their registry so the runner image catalogue can offer the published OS variants.
ARG RELEASE_IMAGE=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.releaseImage=$RELEASE_IMAGE" -o /out/runquill .

FROM docker:29-cli@sha256:4e60c31615e83523afbb59773b25d055c0da9edcd5f80a4c6a6eebad49de1a25
LABEL org.opencontainers.image.licenses="Apache-2.0"
RUN command -v ssh >/dev/null || apk add --no-cache openssh-client
COPY --from=build /out/runquill /usr/local/bin/runquill
COPY LICENSE /usr/share/licenses/runquill/LICENSE
ENTRYPOINT ["/usr/local/bin/runquill"]
