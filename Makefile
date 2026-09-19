SHELL := /bin/bash
BINARY_API     := bin/api
BINARY_MIGRATE := bin/migrate
PKG            := ./...

# Version comes from the VERSION file, which is the single source of truth and
# is reviewed like any other change. `git describe` is not: it reports whatever
# tags happen to exist in the cloning developer's remote, and its old fallback
# to the literal string "dev" meant a mis-built production binary was
# indistinguishable from a laptop build. The commit and build time are stamped
# beside it so a running process can be traced to an exact tree.
VERSION        ?= $(shell cat VERSION 2>/dev/null || echo unknown)
GIT_COMMIT     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
TREE_STATE     ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo clean || echo dirty)
BUILDINFO      := flowed/internal/platform/buildinfo
LDFLAGS        := -X $(BUILDINFO).version=$(VERSION) \
                  -X $(BUILDINFO).commit=$(GIT_COMMIT) \
                  -X $(BUILDINFO).buildTime=$(BUILD_TIME) \
                  -X $(BUILDINFO).treeState=$(TREE_STATE)

# Test database. Kept separate from DB_NAME so `make test-integration` cannot
# drop the database a developer is exploring in another window.
TEST_DB_NAME   ?= flowed_test

# Local development defaults. Override by exporting them or by creating a .env
# file and running `set -a && source .env && set +a` before make.
export DB_HOST     ?= localhost
export DB_PORT     ?= 5432
export DB_USER     ?= $(USER)
export DB_PASSWORD ?=
export DB_NAME     ?= flowed_dev
export DB_SSLMODE  ?= disable
export LOG_FORMAT  ?= text

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/  /'

## build: compile both binaries
build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_API) ./cmd/api
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_MIGRATE) ./cmd/migrate

## run: start the API server
##
## Builds then execs the binary rather than using `go run`. `go run` starts the
## compiled program as a child, and interrupting the parent can leave that child
## alive holding its database pool — which then blocks `make db-drop` with a
## message about "other users" that looks like a permissions problem and is
## really a stray process.
run: build
	exec $(BINARY_API)

## stop: kill any API server left running from an earlier session
stop:
	@pkill -f '$(BINARY_API)' 2>/dev/null && echo "stopped" || echo "nothing running"

## test: run every test
test:
	go test -race -count=1 $(PKG)

## test-short: skip tests that need a database
test-short:
	go test -short -count=1 $(PKG)

## cover: run tests and open the coverage report
cover:
	go test -race -coverprofile=coverage.out -covermode=atomic $(PKG)
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage report written to coverage.html"

## lint: vet and check formatting
lint:
	go vet $(PKG)
	@unformatted=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

## fmt: format the tree
fmt:
	gofmt -w -s .

## tidy: tidy and verify module dependencies
tidy:
	go mod tidy
	go mod verify

## db-create: create the development database
db-create:
	createdb $(DB_NAME) 2>/dev/null || echo "$(DB_NAME) already exists"

## db-drop: drop the development database, disconnecting anything holding it
db-drop:
	@if [ "$(APP_ENV)" = "production" ]; then \
		echo "refusing to drop $(DB_NAME): APP_ENV is production"; exit 1; \
	fi
	@# A dev server left running keeps a pool open, and dropdb then fails with
	@# "being accessed by other users" — which reads like a permissions problem
	@# and is really a stray process. Close the pools first, name what was
	@# closed, and let the drop proceed.
	@closed=$$(psql -tAq postgres -c "\
		SELECT count(pg_terminate_backend(pid)) \
		FROM pg_stat_activity \
		WHERE datname = '$(DB_NAME)' AND pid <> pg_backend_pid()" 2>/dev/null || echo 0); \
	if [ "$$closed" -gt 0 ] 2>/dev/null; then \
		echo "closed $$closed open connection(s) to $(DB_NAME)"; \
	fi
	dropdb --if-exists $(DB_NAME)

## db-reset: drop, recreate and migrate from scratch
db-reset: db-drop db-create migrate-up
	@echo "$(DB_NAME) rebuilt at schema version $$(go run ./cmd/migrate version)"

## migrate-up: apply every pending migration
migrate-up:
	go run ./cmd/migrate up

## migrate-down: roll back the last migration (STEPS=n for more)
migrate-down:
	go run ./cmd/migrate down $(or $(STEPS),1)

## migrate-status: show every migration and its state
migrate-status:
	go run ./cmd/migrate status

## migrate-validate: verify the database matches this build
migrate-validate:
	go run ./cmd/migrate validate

## migrate-new: scaffold a migration pair (NAME=add_something)
migrate-new:
	@test -n "$(NAME)" || (echo "usage: make migrate-new NAME=add_something"; exit 1)
	go run ./cmd/migrate create $(NAME)

## verify-audit: check the audit log hash chain
verify-audit:
	go run ./cmd/migrate verify-audit

## seed: create the first administrator
seed:
	go run ./cmd/api seed

## demo: load a full exploration dataset (refuses on a non-empty database)
demo:
	go run ./cmd/api demo

## demo-reset: rebuild the database and load the demo dataset
demo-reset: db-drop db-create migrate-up demo

## docker-up: start PostgreSQL in docker
docker-up:
	docker compose up -d postgres
	@echo "waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@echo "ready"

## docker-down: stop the docker stack
docker-down:
	docker compose down

## docker-clean: stop the stack and delete its volumes
docker-clean:
	docker compose down -v

## docker-build: build the deployable image, stamped like a local build
##
## The stamp is not decoration: production refuses to serve a build that cannot
## say what it is, and the image asks itself `api version` at build time so a
## mis-stamped image fails here rather than at start-up in a server room.
docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg TREE_STATE=$(TREE_STATE) \
		-t flowed:$(VERSION) -t flowed:latest .

## version: print the build identity this tree would produce
version:
	@echo "version=$(VERSION) commit=$(GIT_COMMIT) built=$(BUILD_TIME) tree=$(TREE_STATE)"

## test-integration: run only the tests that need a real database
test-integration:
	DB_NAME=$(TEST_DB_NAME) go test -race -count=1 ./test/... ./internal/adapter/postgres/...

## test-db-setup: create and migrate the dedicated test database
test-db-setup:
	@createdb $(TEST_DB_NAME) 2>/dev/null || true
	@DB_NAME=$(TEST_DB_NAME) go run ./cmd/migrate up

## test-db-drop: drop the dedicated test database
test-db-drop:
	@dropdb --if-exists $(TEST_DB_NAME)

## staticcheck: run staticcheck if it is installed (CI always installs it)
staticcheck:
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck $(PKG); \
	else \
		echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"; \
		exit 1; \
	fi

## vuln: check dependencies and stdlib against the Go vulnerability database
vuln:
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck $(PKG); \
	else \
		echo "govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@latest"; \
		exit 1; \
	fi

## secrets: refuse obvious secrets committed to the tree
secrets:
	@bash scripts/check-secrets.sh

## openapi: regenerate the API contract from the router
openapi:
	go run ./cmd/openapi api/openapi.json

## openapi-validate: fail if the checked-in contract has drifted from the router
openapi-validate:
	go test -run 'TestOpenAPI|TestEveryRoute|TestPublicRoutes|TestMoneyIsDocumented|TestIdempotentOperations' \
		-count=1 ./internal/adapter/httpapi/...

## ui-build: typecheck, test and build the operator interface into webui/dist
##
## Must run before `make build` for the binary to carry a current interface:
## webui/embed.go embeds the build output, so a binary is only as new as the
## last run of this. A binary built without it serves a page saying so.
ui-build:
	@bash scripts/build-ui.sh

## ui-dev: run the interface with hot reload against a local API on :8080
ui-dev:
	@cd webui && npm run dev

## ui-test: the interface's unit tests, including the tafqit golden vectors
ui-test:
	@cd webui && npx vitest run

## ui-tafqit: regenerate the Arabic spelling vectors from internal/domain/money
##
## The written amount exists in two implementations — Go for the printed
## receipt, TypeScript for the confirmation sheet. This regenerates the golden
## file that keeps them from drifting apart, and must be run after any change
## to internal/domain/money/arabic.go.
ui-tafqit:
	@bash scripts/gen-tafqit.sh

## perf-seed: load the performance dataset (SCALE=small|medium|full)
perf-seed: build
	$(BINARY_API) perf-seed --scale=$(or $(SCALE),small)

## perf: run the performance suite against a seeded database
perf: build
	@bash scripts/perf-run.sh

## backup: take a verified backup (see docs/operations/backup-restore.md)
backup:
	@bash scripts/backup.sh

## restore-drill: prove a backup restores into a scratch database
restore-drill:
	@bash scripts/restore-drill.sh

## audit-verify-external: verify the off-host audit archive
audit-verify-external: build
	$(BINARY_API) audit-ship verify

## check: everything CI runs, in the order CI runs it
check: fmt-check lint staticcheck secrets test-short test-integration migrate-validate

## ci: the full pipeline, exactly as CI executes it
ci: fmt-check lint staticcheck vuln secrets build test-short test-db-setup \
    test-integration migrate-validate openapi-validate ui-build

## fmt-check: fail if anything is unformatted (lint does this too; kept separate for CI clarity)
fmt-check:
	@unformatted=$$(gofmt -l . | grep -v '^vendor/' | grep -v '^web/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: help build run stop test test-short cover lint fmt tidy version \
        db-create db-drop db-reset migrate-up migrate-down migrate-status \
        migrate-validate migrate-new verify-audit seed demo demo-reset \
        docker-up docker-down docker-clean check ci fmt-check staticcheck vuln \
        secrets openapi openapi-validate ui-build perf perf-seed backup restore-drill \
        test-integration test-db-setup test-db-drop audit-verify-external
