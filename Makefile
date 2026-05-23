# tenant-rbac-controller - standard kubebuilder Makefile targets.

# Image URL to use all building/pushing image targets.
IMG ?= tenant-rbac-controller:latest

# Get the currently used golang install path.
GOBIN ?= $(shell go env GOPATH)/bin

# Tool versions.
CONTROLLER_GEN_VERSION ?= v0.15.0
ENVTEST_VERSION       ?= release-0.18
GOLANGCI_LINT_VERSION ?= v1.59.1

CONTROLLER_GEN := $(GOBIN)/controller-gen
ENVTEST        := $(GOBIN)/setup-envtest
GOLANGCI_LINT  := $(GOBIN)/golangci-lint

.PHONY: all
all: build

##@ Development

.PHONY: manifests
manifests: controller-gen
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook \
	  paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint: golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use -p path)" go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet
	go build -o bin/manager ./cmd

.PHONY: run
run: fmt vet
	go run ./cmd

.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push:
	docker push ${IMG}

##@ Deployment

.PHONY: install
install: manifests
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests
	kubectl delete -f config/crd/bases

.PHONY: deploy
deploy: manifests
	kubectl apply -k config/default

.PHONY: undeploy
undeploy:
	kubectl delete -k config/default

##@ Tools

.PHONY: controller-gen
controller-gen:
	test -s $(CONTROLLER_GEN) || \
	  GOBIN=$(GOBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: envtest
envtest:
	test -s $(ENVTEST) || \
	  GOBIN=$(GOBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: golangci-lint
golangci-lint:
	test -s $(GOLANGCI_LINT) || \
	  GOBIN=$(GOBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
