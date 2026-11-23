IMG ?= ghcr.io/rothgar/nodenap:dev
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0

.PHONY: all
all: generate manifests test build

.PHONY: generate
generate: ## Generate deepcopy code.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: ## Generate CRDs and RBAC.
	$(CONTROLLER_GEN) crd rbac:roleName=nodenap paths="./..." \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/*.yaml charts/nodenap/crds/

.PHONY: dashboard
dashboard: ## Regenerate the Grafana dashboard JSON.
	python3 hack/gen-dashboard.py

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/rothgar/nodenap/internal/scaler.Version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/nodenap ./cmd/nodenap

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: run
run: ## Run locally against the current kubeconfig (dry run is controlled by the PowerPolicy).
	go run ./cmd/nodenap --leader-elect=false
