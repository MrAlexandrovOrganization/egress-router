COMPOSE := docker compose -p egress-router -f ../docker-compose.yaml
API := http://127.0.0.1:19091
export NAME URL

.PHONY: init up down status logs fmt fmt-check test refresh add list restart-egress

init:
	@$(MAKE) --no-print-directory -C .. init

up:
	@$(MAKE) --no-print-directory -C .. deploy

down:
	$(COMPOSE) down

status:
	$(COMPOSE) ps

fmt:
	gofmt -w *.go

fmt-check:
	@files="$$(gofmt -l *.go)"; test -z "$$files"

test:
	go test -v ./...

logs:
	$(COMPOSE) logs -f subscription-manager egress-router

list: SHELL := /bin/bash -o pipefail
list:
	@curl --noproxy '*' -fsS $(API)/subscriptions | jq '[.subscriptions[] | {name}]'

refresh:
	@$(MAKE) --no-print-directory -C .. refresh

add:
	@$(MAKE) --no-print-directory -C .. add

restart-egress:
	@$(MAKE) --no-print-directory -C .. refresh
