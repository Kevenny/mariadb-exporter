VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE  ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS     := -ldflags "-s -w -X main.version=$(VERSION) -X main.buildDate=$(BUILD_DATE)"

.PHONY: build test lint docker clean run-dev fmt vet tidy

build:
	go build $(LDFLAGS) -o bin/mariadb_exporter ./cmd/mariadb_exporter/

test:
	go test -v -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t mariadb_exporter:$(VERSION) \
		-t mariadb_exporter:latest \
		.

clean:
	rm -rf bin/ coverage.out

run-dev:
	MARIADB_DSN="mariadb://root:@tcp(localhost:3306)/" \
	go run ./cmd/mariadb_exporter/ --log.level=debug
