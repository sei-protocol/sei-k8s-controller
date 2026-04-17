IMG ?= sei-k8s-controller:latest
GOLANGCI_LINT ?= $(shell which golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

.PHONY: build test lint manifests generate ci docker-build docker-push

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
	cp config/crd/sei.io_seinodes.yaml config/crd/sei.io_seinodedeployments.yaml manifests/
	cp config/rbac/role.yaml manifests/

generate: ## Generate DeepCopy implementations.
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

ci: lint test build ## Run lint, test, and build.

docker-build: ## Build docker image.
	docker build --platform linux/amd64 -t ${IMG} .

docker-push: ## Push docker image.
	docker push ${IMG}
