SHELL := /bin/bash
GO ?= go
BIN ?= bin
VERSION ?= dev
CLUSTER ?= mayfly
LDFLAGS := -s -w -X github.com/hayden/mayfly/internal/version.Version=$(VERSION)
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

image:
	docker build --target mayflyd -t mayflyd:dev .
	docker build --target llmstub -t mayfly-llmstub:dev .
	docker build --target mcpstub -t mayfly-mcpstub:dev .
	docker build --target mayfly-agent -t mayfly-agent:dev .

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
