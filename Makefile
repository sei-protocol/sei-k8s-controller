IMG ?= sei-k8s-controller:latest
GOLANGCI_LINT ?= $(shell which golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

# Pinned tool versions. Bump together: setup-envtest's release branch tracks
# controller-runtime's minor version, and controller-gen pins the schema
# generator that produced the committed config/crd/* manifests.
#
# ENVTEST_K8S_VERSION matches the production EKS control plane (1.34, see
# platform/terraform/.../eks.tf). 1.31 was originally proposed but rejects
# the SeiNode `dataVolume.import` CEL rule — CEL in 1.31 reserves `import`
# as a keyword and refuses oldSelf.import / self.import accessors. 1.32+
# relaxed that.
ENVTEST_K8S_VERSION ?= 1.34.0
SETUP_ENVTEST_VERSION ?= release-0.23
CONTROLLER_GEN_VERSION ?= v0.20.1

LOCALBIN ?= $(CURDIR)/bin
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest

.PHONY: build test test-integration test-all lint manifests generate verify-generated setup-envtest ci docker-build docker-push

build: ## Build manager binary.
	go build -o bin/manager ./cmd/

test: ## Run tests.
	go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

lint: ## Run golangci-lint.
	$(GOLANGCI_LINT) run

manifests: ## Generate CRD and RBAC manifests.
	controller-gen rbac:roleName=manager-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac
	cp config/crd/sei.io_seinodes.yaml config/crd/sei.io_seinetworks.yaml config/crd/sei.io_seinodetasks.yaml manifests/
	cp config/rbac/role.yaml manifests/

generate: ## Generate DeepCopy implementations.
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

setup-envtest: $(LOCALBIN) ## Install setup-envtest and download K8s test binaries.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

test-integration: setup-envtest ## Run envtest-tagged integration tests.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -tags=envtest -timeout=10m \
			./internal/controller/seinetwork/envtest/... \
			./internal/controller/node/envtest/... \
			./internal/controller/nodetask/envtest/...

test-all: test test-integration ## Run unit + integration tests.

verify-generated: manifests generate ## Fail if generated artifacts drift from committed state.
	@git diff --exit-code -- config/ manifests/ api/ || { \
		echo "ERROR: generated artifacts out of date — run 'make manifests generate' and commit"; \
		exit 1; }

ci: lint test verify-generated build ## Run lint, test, verify-generated, and build.

docker-build: ## Build docker image.
	docker build --platform linux/amd64 -t ${IMG} .

docker-push: ## Push docker image.
	docker push ${IMG}
