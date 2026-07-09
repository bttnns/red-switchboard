# red-switchboard developer tasks.
# Run inside the Fedora `dev` container (see AGENTS.md): toolchain lives there.
#
#   make            # vet + build + test (the common loop)
#   make lint       # golangci-lint (auto-installs the CI-pinned version)
#   make ci         # everything CI runs: vet, build, test -race, lint

GOLANGCI_LINT_VERSION := v2.12.2
GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint

.DEFAULT_GOAL := check

.PHONY: check ci vet build test test-race integration lint fmt tidy clean tools

## check: vet + build + test (fast inner loop)
check: vet build test

## ci: mirror the GitHub CI jobs locally
ci: vet build test-race lint

## vet: go vet over all packages
vet:
	go vet ./...

## build: compile all packages
build:
	go build ./...

## test: run the test suite
test:
	go test ./...

## test-race: run tests with the race detector (as CI does)
test-race:
	go test -race ./...

## integration: the compose-based TeslaMate end-to-end test (needs Docker; not in CI's default)
integration:
	go test -tags integration -count=1 -timeout 30m ./test/integration/...

## lint: run golangci-lint (installs the CI-pinned version if missing)
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

## fmt: format all Go sources
fmt:
	gofmt -w .

## tidy: tidy go.mod / go.sum
tidy:
	go mod tidy

## tools: install pinned dev tooling (golangci-lint)
tools: $(GOLANGCI_LINT)

$(GOLANGCI_LINT):
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

## clean: remove build/test caches
clean:
	go clean -cache -testcache
