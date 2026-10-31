# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/version-monitor ./cmd/version-monitor

# Static binary, CA bundle for the HTTPS calls, nothing else. Runs as nobody.
FROM scratch
ARG VERSION=dev
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="version-monitor" \
      org.opencontainers.image.description="Prometheus exporter for version drift and end-of-life across a Kubernetes cluster" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/dmleet/version-monitor" \
      maintainer="dmleet"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/version-monitor /version-monitor
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/version-monitor"]
