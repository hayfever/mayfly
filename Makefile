SHELL := /bin/bash
GO ?= go
BIN ?= bin
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CLUSTER ?= mayfly
LDFLAGS := -s -w -X github.com/hayfever/mayfly/internal/version.Version=$(VERSION) -X github.com/hayfever/mayfly/internal/version.Commit=$(COMMIT) -X github.com/hayfever/mayfly/internal/version.Date=$(DATE)
CMDS := mayflyd mayfly-agent mayfly mayfly-harness llmstub mcpstub

.PHONY: build test vet fmt image kind-create kind-up kind-down deploy e2e run-local clean FORCE

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
	gofmt -w cmd internal test

IMAGE_ARGS := --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE)

image:
	docker build $(IMAGE_ARGS) --target mayflyd -t mayflyd:dev .
	docker build $(IMAGE_ARGS) --target llmstub -t mayfly-llmstub:dev .
	docker build $(IMAGE_ARGS) --target mcpstub -t mayfly-mcpstub:dev .
	docker build $(IMAGE_ARGS) --target mayfly-agent -t mayfly-agent:dev .

kind-create:
	kind create cluster --config deploy/kind.yaml

kind-up:
	./hack/kind-up.sh

kind-down:
	kind delete cluster --name $(CLUSTER)

deploy:
	kubectl kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/kind | kubectl apply -f -

e2e:
	./hack/e2e-kind.sh

run-local:
	./hack/run-local.sh

clean:
	rm -rf $(BIN) .mayfly
