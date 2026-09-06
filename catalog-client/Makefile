.PHONY: build fmt fmt-check lint test check generate

MODULE := github.com/MrAlexandrovOrganization/egress-router/catalog-client
PROTO := catalog/v1/catalog.proto

build:
	go build -trimpath -o bin/catalog-shadow ./cmd/catalog-shadow

fmt:
	gofmt -w cmd internal/shadow internal/nodeuri

fmt-check:
	@files="$$(gofmt -l cmd internal/shadow internal/nodeuri)" || exit; test -z "$$files"

lint:
	go vet ./...

test:
	go test -race -cover -timeout 120s ./...

check: fmt-check lint test

# protoc 34.0; pinned plugins and local M mappings override the owner's go_package.
generate:
	@test "$$(protoc --version)" = 'libprotoc 34.0'
	mkdir -p .tools internal/gen/catalogv1
	GOBIN=$(CURDIR)/.tools go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
	GOBIN=$(CURDIR)/.tools go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	PATH="$(CURDIR)/.tools:$$PATH" protoc -I api/proto --go_out=. --go_opt=module=$(MODULE) --go_opt=M$(PROTO)=$(MODULE)/internal/gen/catalogv1 --go-grpc_out=. --go-grpc_opt=module=$(MODULE) --go-grpc_opt=M$(PROTO)=$(MODULE)/internal/gen/catalogv1 $(PROTO)
