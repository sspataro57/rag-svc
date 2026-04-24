.PHONY: build test lint run migrate seed tidy compose-up compose-down help

BINARY := rag-svc
PKG    := ./...

help:
	@echo "Targets:"
	@echo "  build         Build the rag-svc binary to ./bin/$(BINARY)"
	@echo "  test          Run unit tests"
	@echo "  lint          Run go vet and gofmt check"
	@echo "  run           Run the server (reads .env via compose or shell)"
	@echo "  migrate       Apply database migrations"
	@echo "  seed          (Stub) Load fixture data — filled in at a later milestone"
	@echo "  tidy          go mod tidy"
	@echo "  compose-up    docker compose up (builds + starts postgres/redis/minio/rag-svc)"
	@echo "  compose-down  docker compose down"

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/$(BINARY) ./cmd/rag-svc

test:
	go test $(PKG)

lint:
	go vet $(PKG)
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "gofmt needs to run on:"; echo "$$files"; exit 1; \
	fi

run: build
	./bin/$(BINARY) serve

migrate: build
	./bin/$(BINARY) migrate

seed:
	@echo "seed: not implemented yet — ships with the Jira ingestion milestone"

tidy:
	go mod tidy

compose-up:
	docker compose -f deploy/compose/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down -v
