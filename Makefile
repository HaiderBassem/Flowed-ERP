SHELL := /bin/bash
BINARY_API     := bin/api
BINARY_MIGRATE := bin/migrate
PKG            := ./...
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS        := -X main.version=$(VERSION)

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

## check: everything CI runs
check: lint test migrate-validate

.PHONY: help build run stop test test-short cover lint fmt tidy \
        db-create db-drop db-reset migrate-up migrate-down migrate-status \
        migrate-validate migrate-new verify-audit seed demo demo-reset \
        docker-up docker-down docker-clean check
