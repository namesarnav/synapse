SHELL := /bin/bash
GO ?= go
export SYNAPSE_TEST_ADMIN_URL ?= postgres://synapse:synapse@localhost:55432/postgres?sslmode=disable
export SYNAPSE_DATABASE_URL ?= postgres://synapse:synapse@localhost:55432/synapse?sslmode=disable
export SYNAPSE_REDIS_URL ?= redis://localhost:56379/0

.PHONY: help dev-infra dev-infra-down dev build test race lint fmt integration e2e load-test benchmark web-install web-test web-build docker-up docker-down chaos

help:
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

dev-infra: ## start Postgres + Redis for development
	docker compose -f docker-compose.dev.yml up -d --wait

dev-infra-down: ## stop dev infrastructure
	docker compose -f docker-compose.dev.yml down -v

dev: dev-infra ## run api (with scheduler) + 2 workers + web dev server
	@trap 'kill 0' EXIT; \
	SYNAPSE_HTTP_ALLOW_PRIVATE=true $(GO) run ./apps/api & \
	SYNAPSE_WORKER_HTTP_ADDR=:8091 SYNAPSE_HTTP_ALLOW_PRIVATE=true $(GO) run ./apps/worker & \
	SYNAPSE_WORKER_HTTP_ADDR=:8092 SYNAPSE_HTTP_ALLOW_PRIVATE=true $(GO) run ./apps/worker & \
	cd apps/web && npm run dev

build: ## build all binaries into ./bin
	mkdir -p bin && $(GO) build -o bin/ ./apps/api ./apps/worker ./apps/scheduler

test: ## unit tests (integration tests skip when Postgres is down)
	$(GO) test ./internal/... ./packages/... 

race: ## full Go test suite with the race detector
	SYNAPSE_STACK_RACE=1 SYNAPSE_REQUIRE_DB=1 $(GO) test -race -count=1 -timeout 15m ./internal/... ./tests/...

lint: ## gofmt, vet, staticcheck, tsc, eslint
	@test -z "$$(gofmt -l apps internal tests migrations packages)" || (gofmt -l apps internal tests migrations packages; exit 1)
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...
	cd apps/web && npm run lint

fmt:
	gofmt -w apps internal tests migrations packages

integration: dev-infra ## integration + failure-injection tests
	SYNAPSE_REQUIRE_DB=1 $(GO) test -count=1 -timeout 10m ./tests/integration/...

e2e: dev-infra ## end-to-end tests through the public webhook endpoint
	SYNAPSE_REQUIRE_DB=1 $(GO) test -count=1 -timeout 10m ./tests/e2e/...

load-test: dev-infra ## reproducible load tests (see docs/benchmarks)
	tests/load/suite.sh

benchmark: dev-infra ## Go micro-benchmarks (expressions, graph, queue)
	SYNAPSE_REQUIRE_DB=1 $(GO) test -run xxx -bench . -benchmem ./internal/expressions ./internal/engine ./internal/realtime ./internal/runtime

chaos: dev-infra ## worker-kill recovery demo
	./scripts/chaos-demo.sh

web-install:
	cd apps/web && npm ci

web-test:
	cd apps/web && npm test

web-build:
	cd apps/web && npm run build

docker-up: ## full stack in containers
	docker compose up -d --build --wait

docker-down:
	docker compose down -v
