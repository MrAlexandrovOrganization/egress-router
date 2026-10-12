.PHONY: init up build deploy down logs status fmt fmt-check shell-check config-check compose-check test check install-tproxy refresh
export NAME URL
export EGRESS_UID EGRESS_GID

COMPOSE := docker compose -p egress-router -f docker-compose.yaml
include versions.mk

init:
	@bash scripts/deploy.sh init

up: deploy

build:
	$(COMPOSE) build

deploy:
	@bash scripts/deploy.sh deploy

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f subscription-manager egress-router

status:
	$(COMPOSE) ps

fmt:
	gofmt -w subscription-manager/*.go
	$(MAKE) -C catalog-client fmt

fmt-check:
	@files="$$(gofmt -l subscription-manager/*.go)" || exit; test -z "$$files"
	$(MAKE) -C catalog-client fmt-check

shell-check:
	sh -n telemt-egress-tproxy.sh
	@for script in scripts/*.sh; do bash -n "$$script" || exit; done

config-check:
	docker run --rm -i -v "$(PWD)/config.json.example:/config.json:ro" $(SING_BOX_IMAGE) check -c /config.json
	SING_BOX_TEST_IMAGE='$(SING_BOX_IMAGE)' go -C subscription-manager test -run TestIntegration -v ./...
	SING_BOX_TEST_IMAGE='$(SING_BOX_IMAGE)' go -C catalog-client test -timeout 120s -run TestIntegration -v ./...

compose-check:
	$(COMPOSE) config --quiet

test:
	python3 -m unittest discover -s scripts -p 'test_*.py'
	go -C subscription-manager test -race -cover ./...
	go -C subscription-manager vet ./...
	$(MAKE) -C catalog-client test lint

.PHONY: shadow-check
shadow-check:
	$(MAKE) -C catalog-client check build

check: fmt-check shell-check config-check compose-check test

install-tproxy:
	sudo install -m 0755 telemt-egress-tproxy.sh /usr/local/sbin/telemt-egress-tproxy
	sudo install -m 0644 telemt-egress-tproxy.service /etc/systemd/system/telemt-egress-tproxy.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now telemt-egress-tproxy.service

refresh:
	@bash scripts/deploy.sh refresh

.PHONY: add rollback deployment-smoke
add:
	@bash scripts/deploy.sh add

rollback:
	@bash scripts/deploy.sh rollback

deployment-smoke:
	@bash scripts/deploy-smoke.sh

.PHONY: versions
versions:
	@$(foreach v,$(VERSION_VARS),printf '%s=%s\n' '$(v)' '$($(v))';)

.PHONY: compose
ARGS ?= ps
compose:
	$(COMPOSE) $(ARGS)

# CI builds/publishes once; VM activates immutable digests (Python 3 + Docker required).
export IMAGE_COMPOSE_FILE := docker-compose.yaml
export IMAGE_PROJECT_NAME := egress-router
.PHONY: image-build image-publish image-deploy image-activate
image-build:
	python3 scripts/images.py build
image-publish:
	python3 scripts/images.py publish
image-deploy:
	python3 scripts/images.py deploy
image-activate:
	IMAGE_COMPOSE="$(IMAGE_COMPOSE)" IMAGE_NO_BUILD=1 bash scripts/deploy.sh deploy

.PHONY: image-check
image-check:
	python3 -B scripts/test_images.py

.PHONY: image-compose
export IMAGE_ARGS
image-compose:
	python3 scripts/images.py compose

include makefiles/ci-deploy.mk
