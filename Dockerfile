# The builder runs on the build host's native platform and cross-compiles to
# the target one (pure Go, CGO off), so a multi-arch build doesn't need QEMU:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t mariadb_exporter .
#
# The base image is pinned by digest for reproducible builds; Dependabot bumps it.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

ARG VERSION=dev
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /build

# Dependencies are downloaded in their own layer to take advantage of caching
# when only the source code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
    -o mariadb_exporter ./cmd/mariadb_exporter/

FROM scratch

COPY --from=builder /build/mariadb_exporter /mariadb_exporter
# CA certificates for TLS connections to MariaDB.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 9104
USER 65534

# The image has no shell or curl; the binary probes itself. With TLS or basic
# auth enabled, override it with --url (or MARIADB_HEALTHCHECK_URL).
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s --retries=3 \
    CMD ["/mariadb_exporter", "healthcheck"]

ENTRYPOINT ["/mariadb_exporter"]
