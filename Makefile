BIN := bin
GO := go

.PHONY: build test vet run-demo demo tidy docker

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

# Demo: broken site on :6060/:6161, daemon on :8080 (in-memory store).
run-demo: build
	./$(BIN)/brokensite -http :6060 -https :6161 &
	sleep 1
	./$(BIN)/synthusersd -config config.example.yaml

docker:
	docker compose up -d --build
