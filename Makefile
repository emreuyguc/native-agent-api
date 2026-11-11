MODULE := $(shell go list -m)
BINARY := $(notdir $(MODULE))
ENV_FILE := .env
VERSION ?= $(shell git describe --tags --always --dirty)
COMMIT ?= $(shell git rev-parse --verify -q HEAD)
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.Date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format Go files
	golangci-lint fmt ./...

.PHONY: lint
lint: ## Run static analysis
	golangci-lint run ./...

.PHONY: test
test: ## Run tests with the race detector
	go test -race -count=1 ./...

.PHONY: coverage
coverage: ## Measure test coverage and check it against .testcoverage.yml
	go test -count=1 -coverprofile=coverage.out ./...
	go tool -modfile=tools/go.mod go-test-coverage --config=.testcoverage.yml

.PHONY: test-host
test-host: ## Run host tests against the installed CLI and its native login (AGENT=claude|codex)
	NATIVE_AGENT_HOST_AGENT=$(AGENT) go test -tags host -count=1 -v ./internal/hosttest

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each (for example FUZZTIME=1m)
	@test -n "$(FUZZTIME)" || { echo "pass FUZZTIME=<duration>, for example FUZZTIME=1m"; exit 1; }
	@for pkg in $$(go list ./...); do \
	  for target in $$(go test -list '^Fuzz' "$$pkg" | grep '^Fuzz'); do \
	    echo "$$pkg $$target"; \
	    go test -run '^$$' -fuzz "^$$target$$" -fuzztime "$(FUZZTIME)" "$$pkg" || exit 1; \
	  done; \
	done

.PHONY: tidy
tidy: ## Verify module metadata of the product and of tools/
	go mod tidy
	go mod verify
	cd tools && go mod tidy && go mod verify

.PHONY: lint-workflows
lint-workflows: ## Check the GitHub Actions workflows
	go tool -modfile=tools/go.mod actionlint

.PHONY: vuln
vuln: ## Scan Go dependencies for known vulnerabilities
	go tool govulncheck ./...

.PHONY: api-diff
api-diff: ## List API changes that break clients, against BASE (for example BASE=origin/main)
	@test -n "$(BASE)" || { echo "pass BASE=<revision>, for example BASE=origin/main"; exit 1; }
	scripts/api-diff.sh "$(BASE)"

.PHONY: check
check: tidy lint lint-workflows test vuln ## Run all repository checks

.PHONY: build
build: ## Build the local binary
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: run
run: build ## Build and serve with the local .env (override with ENV_FILE=path)
	@test -f "$(ENV_FILE)" || { echo "$(ENV_FILE) not found: copy .env.example to $(ENV_FILE) and edit it, or pass ENV_FILE=path"; exit 1; }
	./bin/$(BINARY) serve --env-file "$(ENV_FILE)"

.PHONY: setup
setup: ## Set up an installation interactively: host or Docker, env files, client token
	@scripts/setup.sh

.PHONY: clean
clean: ## Remove local build output
	rm -rf bin
