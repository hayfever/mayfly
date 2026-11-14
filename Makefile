SHELL := /bin/bash
GO ?= go
BIN ?= bin
VERSION ?= 0.1.0
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CLUSTER ?= mayfly
LDFLAGS := -s -w -X github.com/hayfever/mayfly/internal/version.Version=$(VERSION) -X github.com/hayfever/mayfly/internal/version.Commit=$(COMMIT) -X github.com/hayfever/mayfly/internal/version.Date=$(DATE)
CMDS := mayflyd mayfly-agent mayfly mayfly-harness mayfly-plugin-shim llmstub mcpstub

.PHONY: build test vet fmt image cluster-create cluster-up cluster-down deploy e2e e2e-up e2e-test e2e-down run-local clean FORCE

build: $(addprefix $(BIN)/,$(CMDS))

$(BIN)/%: FORCE
	@mkdir -p $(BIN)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/$*

FORCE:

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal test e2e

IMAGE_ARGS := --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE)

image:
	docker build $(IMAGE_ARGS) --target mayflyd -t mayflyd:dev .
	docker build $(IMAGE_ARGS) --target llmstub -t mayfly-llmstub:dev .
	docker build $(IMAGE_ARGS) --target mcpstub -t mayfly-mcpstub:dev .
	docker build $(IMAGE_ARGS) --target mayfly-agent -t mayfly-agent:dev .

# Quickstart cluster on k3d; see docs/deployment.md.
cluster-create:
	k3d cluster create $(CLUSTER) --config deploy/k3d.yaml --wait --kubeconfig-switch-context=false

cluster-up:
	./hack/k3d-up.sh

cluster-down:
	k3d cluster delete $(CLUSTER)

deploy:
	kubectl --context k3d-$(CLUSTER) kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/local | kubectl --context k3d-$(CLUSTER) apply -f -

# End-to-end suite on k3d; see docs/testing.md.
e2e:
	./hack/e2e.sh all

e2e-up:
	./hack/e2e.sh up

e2e-test:
	./hack/e2e.sh test

e2e-down:
	./hack/e2e.sh down

run-local:
	./hack/run-local.sh

clean:
	rm -rf $(BIN) .mayfly .e2e
