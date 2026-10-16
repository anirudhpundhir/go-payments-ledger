TEST_DB ?= postgres://postgres:postgres@localhost:5432/ledger_test?sslmode=disable

.PHONY: build run test test-unit test-integration test-serializable bench lint vet up down clean

build:            ## Build the server binary
	go build -o bin/ledgerd ./cmd/ledgerd

run: build        ## Run against DATABASE_URL (default: local compose Postgres)
	./bin/ledgerd

up:               ## Start Postgres + ledgerd via docker compose
	docker compose up -d --build

down:
	docker compose down -v

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test-unit:        ## Unit tests only (no database needed)
	go test -race -count=1 ./internal/ledger ./internal/api ./internal/idempotency

test-integration: ## Integration tests against $(TEST_DB)
	LEDGER_TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 -v ./internal/integration

test-serializable: ## Integration tests with SERIALIZABLE isolation
	LEDGER_TEST_ISOLATION=serializable LEDGER_TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 ./internal/integration

test:             ## All tests
	LEDGER_TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 ./...

bench:            ## End-to-end HTTP benchmarks (needs $(TEST_DB))
	LEDGER_TEST_DATABASE_URL="$(TEST_DB)" go test -run '^$$' -bench . -benchtime 3s ./internal/integration

clean:
	rm -rf bin
