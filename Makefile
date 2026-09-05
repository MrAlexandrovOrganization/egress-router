.PHONY: init up down logs status config-check compose-check test check install-tproxy refresh

COMPOSE := docker compose -p egress-router -f docker-compose.yaml
SING_BOX_IMAGE := ghcr.io/sagernet/sing-box:v1.14.0@sha256:4bed9332a0013fef72c31200a84e8fc0ed91a5ab2fe373a69f0acbbbbfbef3c5

init:
	@test -e config.json || cp config.json.example config.json
	@test -e subscription-manager/subscriptions.json || cp subscription-manager/subscriptions.json.example subscription-manager/subscriptions.json
	mkdir -p runtime

up: init
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f subscription-manager egress-router

status:
	$(COMPOSE) ps

config-check:
	python3 -m json.tool config.json.example >/dev/null
	docker run --rm -i -v "$(PWD)/config.json.example:/config.json:ro" $(SING_BOX_IMAGE) check -c /config.json

compose-check:
	$(COMPOSE) config --quiet

test:
	python3 -m unittest discover -s subscription-manager/tests -v

check: config-check compose-check test

install-tproxy:
	sudo install -m 0755 telemt-egress-tproxy.sh /usr/local/sbin/telemt-egress-tproxy
	sudo install -m 0644 telemt-egress-tproxy.service /etc/systemd/system/telemt-egress-tproxy.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now telemt-egress-tproxy.service

refresh:
	$(MAKE) -C subscription-manager refresh
