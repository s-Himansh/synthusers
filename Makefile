BIN := bin
GO := go
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: build test vet run run-demo up down tidy

build:
	mkdir -p $(BIN)
	$(GO) build -o $(BIN)/synthusersd ./cmd/synthusersd
	$(GO) build -o $(BIN)/brokensite ./cmd/brokensite

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -timeout 10m

tidy:
	$(GO) mod tidy

# Your own sites: copy configs/example.yaml to configs/local.yaml (git-ignored).
run: build
	./$(BIN)/synthusersd -config configs/local.yaml

# Demo: broken site on :6060/:6161, daemon on :8080 (in-memory store).
run-demo: build
	./$(BIN)/brokensite -http :6060 -https :6161 &
	sleep 1
	./$(BIN)/synthusersd -config configs/example.yaml

# Full stack: daemon + Postgres + headless Chrome + demo site.
up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down
