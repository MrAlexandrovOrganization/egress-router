COMPOSE := docker compose -p egress-router -f ../docker-compose.yaml
API := http://127.0.0.1:19091

.PHONY: init up down status logs fmt fmt-check test refresh add list restart-egress

init:
	@test -e subscriptions.json || cp subscriptions.json.example subscriptions.json

up: init
	$(COMPOSE) up -d --build

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

list:
	curl -fsS $(API)/subscriptions

refresh:
	curl -fsS -X POST $(API)/refresh
	$(MAKE) restart-egress

add:
	@test -n "$(NAME)" || (echo "usage: make add NAME=name URL=https://..." >&2; exit 2)
	@test -n "$(URL)" || (echo "usage: make add NAME=name URL=https://..." >&2; exit 2)
	curl -fsS -X POST $(API)/subscriptions -H 'Content-Type: application/json' --data "$$(jq -n --arg name '$(NAME)' --arg url '$(URL)' '{name:$$name,url:$$url}')"
	$(MAKE) restart-egress

restart-egress:
	$(COMPOSE) up -d --no-deps --force-recreate egress-router
