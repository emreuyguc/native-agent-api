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

# Development tools. Go tools have their own modules in tools/; the other tools run from the image
# in tools/docker/<name>/Dockerfile. Dependabot updates both, and CI runs the same versions.
GOLANGCI_LINT := go tool -modfile=tools/golangci-lint/go.mod golangci-lint
GORELEASER := go tool -modfile=tools/goreleaser/go.mod goreleaser
TOOLS := go tool -modfile=tools/go.mod
GO_MODULES := . tools tools/golangci-lint tools/goreleaser
tool_image = $(shell sed -n 's/^FROM //p' tools/docker/$(1)/Dockerfile)
IMAGES := claude codex edge

.DEFAULT_GOAL := help

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format Go files
	$(GOLANGCI_LINT) fmt ./...

.PHONY: lint
lint: ## Run static analysis
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: ## Run tests with the race detector and write coverage.out
	go test -race -count=1 -coverprofile=coverage.out ./...

.PHONY: coverage
coverage: test ## Check the test coverage against .testcoverage.yml; writes coverage.txt
	@status=0; $(TOOLS) go-test-coverage --config=.testcoverage.yml >coverage.txt || status=$$?; \
	  cat coverage.txt; exit $$status

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
tidy: ## Tidy the product module and every tool module
	@for m in $(GO_MODULES); do (cd $$m && go mod tidy) || exit 1; done

.PHONY: modules
modules: ## Check that every module is tidy and matches go.sum
	@for m in $(GO_MODULES); do echo "$$m"; (cd $$m && go mod tidy -diff && go mod verify) || exit 1; done

.PHONY: lint-workflows
lint-workflows: ## Check the GitHub Actions workflows (actionlint)
	$(TOOLS) actionlint

# Without a GitHub token zizmor runs offline. CI passes ZIZMOR_ONLINE=1 and GH_TOKEN to also look
# up known vulnerable actions; GH_TOKEN reaches the container only then.
.PHONY: lint-zizmor
lint-zizmor: ## Find unsafe patterns in the workflows and Dependabot config (zizmor, Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -w /src $(if $(ZIZMOR_ONLINE),-e GH_TOKEN,) \
	  $(call tool_image,zizmor) $(if $(ZIZMOR_ONLINE),,--offline) .

.PHONY: lint-shell
lint-shell: ## Check the shell scripts (ShellCheck, Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -w /src $(call tool_image,shellcheck) \
	  $$(git ls-files '*.sh' .githooks)

.PHONY: vuln
vuln: ## Scan Go dependencies for known vulnerabilities
	go tool govulncheck ./...

.PHONY: api-diff
api-diff: ## List API changes that break clients, against BASE (for example BASE=origin/main)
	@test -n "$(BASE)" || { echo "pass BASE=<revision>, for example BASE=origin/main"; exit 1; }
	scripts/api-diff.sh "$(BASE)"

.PHONY: images
images: ## Build the Docker images without pushing (IMAGE=claude|codex|edge, PLATFORMS=<list>)
	@for image in $(or $(IMAGE),$(IMAGES)); do \
	  echo "image $$image"; \
	  docker buildx build --file docker/$$image/Dockerfile $(if $(PLATFORMS),--platform $(PLATFORMS),) . || exit 1; \
	done

.PHONY: snapshot
snapshot: ## Build the release archives into dist/ without publishing (no SBOM)
	$(GORELEASER) release --snapshot --clean --skip=publish,sbom

# The two levels, named after when they run. `make hooks` makes git run them; CI runs the same
# targets. docs/development.md lists which CI job runs which target.
.PHONY: check-pre-commit
check-pre-commit: modules lint lint-workflows coverage vuln ## Before a commit: Go checks, tests and coverage (needs only Go)
	@echo "check-pre-commit: OK"

.PHONY: check-pre-push
check-pre-push: check-pre-commit lint-zizmor lint-shell api-diff fuzz images snapshot ## Before a push: every CI check that runs locally (needs Docker)
	@echo "check-pre-push: OK"

check-pre-push: BASE ?= origin/main
check-pre-push: FUZZTIME ?= 10s

.PHONY: hooks
hooks: ## Make git run check-pre-commit before each commit and check-pre-push before each push
	git config --local core.hooksPath .githooks
	@echo "hooks: .githooks (undo with: git config --local --unset core.hooksPath)"

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
