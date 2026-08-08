FROM golang:1.22-alpine AS builder

ARG VERSION=dev
ARG BUILD_DATE=unknown

WORKDIR /build

# As dependências são baixadas em uma camada própria para aproveitar o cache
# quando apenas o código-fonte muda.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
    -o mariadb_exporter ./cmd/mariadb_exporter/

FROM scratch

COPY --from=builder /build/mariadb_exporter /mariadb_exporter
# Certificados de CA para conexões TLS ao MariaDB.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 9104
USER 65534

ENTRYPOINT ["/mariadb_exporter"]
