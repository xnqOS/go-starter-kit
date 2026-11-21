-include .env
export

IMAGE_NAME ?= starter-kit:local

.PHONY: help init run build test check fmt-check module migrate db-up db-down logs-up logs-down logs-tail logs-test image deploy-config deploy deploy-down deploy-logs

help:
	@echo 'make init MODULE=github.com/acme/my-api  Rename this project'
	@echo 'make init MODULE=github.com/acme/my-api DEST=../my-api  Export a clean copy'
	@echo 'make run                         Start the API (SQLite by default; migrate first)'
	@echo 'make build                       Build bin/api and bin/migrate'
	@echo 'make module NAME=order           Generate and register a CRUD module'
	@echo 'make module NAME=order ROUTE=/orders'
	@echo 'make db-up                       Start local PostgreSQL'
	@echo 'make migrate                     Apply pending SQLite/PostgreSQL Go migrations'
	@echo 'make test                        Run tests with the race detector'
	@echo 'make check                       Run gofmt check, tests, vet, and build'
	@echo 'make logs-up                     Start VictoriaLogs (UI: http://localhost:9428/select/vmui/)'
	@echo 'make run LOG_OUTPUTS=stdout,syslog LOG_SERVICE=order-api'
	@echo 'make logs-test                   Test two apps against the local log server'
	@echo 'make deploy-config               Local page to write .env.deploy (explains and checks every setting)'
	@echo 'make deploy                      Build image, migrate, then start API (copy .env.deploy.example first)'
	@echo 'make deploy-down                 Stop deployed API and preserve data'
	@echo 'make deploy-logs                 Follow deployed API logs'

init:
	go run ./cmd/projectinit -module "$$MODULE" -dest "$$DEST"

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate

test:
	go test -race ./...
	@if [ -d kit ]; then echo 'cd kit && go test -race ./...'; cd kit && go test -race ./...; fi

fmt-check:
	@files="$$(gofmt -l .)" || exit $$?; if [ -n "$$files" ]; then echo "Run gofmt on:"; echo "$$files"; exit 1; fi

check: fmt-check test
	go vet ./...
	go build ./...
	@if [ -d kit ]; then echo 'cd kit && go vet ./... && go build ./...'; cd kit && go vet ./... && go build ./...; fi

module:
	@./scripts/add-module.sh "$(NAME)" -path "$(ROUTE)"

migrate:
	go run ./cmd/migrate

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose stop postgres

logs-up:
	docker compose --profile logging up -d --wait victorialogs

logs-down:
	docker compose --profile logging stop victorialogs

logs-tail:
	docker compose --profile logging logs --tail=100 -f victorialogs

logs-test:
	VICTORIALOGS_TEST_URL=http://127.0.0.1:9428 VICTORIALOGS_TEST_SYSLOG_ADDRESS=127.0.0.1:5140 go test github.com/xnqOS/go-starter-kit/kit/logging -run TestVictoriaLogsIntegration -count=1 -v

image:
	docker build -t "$$IMAGE_NAME" .

deploy-config:
	go run ./cmd/deploy-config

deploy:
	docker compose -f compose.deploy.yaml build api
	docker compose -f compose.deploy.yaml run --rm --no-deps migrate
	docker compose -f compose.deploy.yaml up -d --no-deps --wait api

deploy-down:
	docker compose -f compose.deploy.yaml stop api

deploy-logs:
	docker compose -f compose.deploy.yaml logs --tail=100 -f api
