# Resolve paths relative to this file, including from nested Makefiles.
VERSIONS_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
GO_VERSION := $(shell awk '$$1 == "go" {print $$2; exit}' $(VERSIONS_ROOT)subscription-manager/go.mod)
CATALOG_GO_VERSION := $(shell awk '$$1 == "go" {print $$2; exit}' $(VERSIONS_ROOT)catalog-client/go.mod)
PROTOC_GEN_GO_VERSION := $(shell awk '$$1 == "google.golang.org/protobuf" {print $$2; exit}' $(VERSIONS_ROOT)catalog-client/go.mod)
PROTOC_VERSION := 34.0
PROTOC_GEN_GO_GRPC_VERSION := 1.5.1
ALPINE_VERSION := 3.21
SMOKE_IMAGE := debian:bookworm-slim
SING_BOX_IMAGE := ghcr.io/sagernet/sing-box:v1.14.0@sha256:4bed9332a0013fef72c31200a84e8fc0ed91a5ab2fe373a69f0acbbbbfbef3c5
VERSION_VARS := GO_VERSION CATALOG_GO_VERSION PROTOC_GEN_GO_VERSION PROTOC_VERSION PROTOC_GEN_GO_GRPC_VERSION ALPINE_VERSION SING_BOX_IMAGE SMOKE_IMAGE
export $(VERSION_VARS)
