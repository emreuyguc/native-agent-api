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
# CI installs the same golangci-lint version as a binary and passes GOLANGCI_LINT=golangci-lint.
GOLANGCI_LINT ?= go tool -modfile=tools/golangci-lint/go.mod golangci-lint
TOOLS := go tool -modfile=tools/go.mod
GO_MODULES := . tools tools/golangci-lint
tool_image = $(shell sed -n 's/^FROM //p' tools/docker/$(1)/Dockerfile)
IMAGES := claude codex edge

.DEFAULT_GOAL := help

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: format-code
format-code: ## Format Go files
	$(GOLANGCI_LINT) fmt ./...

.PHONY: lint-go
lint-go: ## Run static analysis
	$(GOLANGCI_LINT) run ./...

# Locally Go reuses the results of unchanged packages. CI passes TESTFLAGS=-count=1 to run all.
.PHONY: test
test: ## Run tests with the race detector and write coverage.out (TESTFLAGS=-count=1 runs all)
	go test -race $(TESTFLAGS) -coverprofile=coverage.out ./...

.PHONY: check-coverage
check-coverage: test ## Check the test coverage against .testcoverage.yml; writes coverage.txt
	@status=0; $(TOOLS) go-test-coverage --config=.testcoverage.yml >coverage.txt || status=$$?; \
	  cat coverage.txt; exit $$status

# The fuzz workflow runs one job per line.
.PHONY: list-fuzz-targets
list-fuzz-targets: ## List the fuzz targets, one "<package> <target>" per line
	@for pkg in $$(go list ./...); do \
	  go test -list '^Fuzz' "$$pkg" | sed -n "s|^Fuzz|$$pkg Fuzz|p"; \
	done

.PHONY: test-host
test-host: ## Run host tests against the installed CLI and its native login (AGENT=claude|codex)
	NATIVE_AGENT_HOST_AGENT=$(AGENT) go test -tags host -count=1 -v ./internal/hosttest

.PHONY: test-fuzz
test-fuzz: ## Run every fuzz target for FUZZTIME each (FUZZ_PKG=<package> FUZZ_TARGET=<name> pick one)
	@test -n "$(FUZZTIME)" || { echo "pass FUZZTIME=<duration>, for example FUZZTIME=1m"; exit 1; }
	@for pkg in $$(go list $(or $(FUZZ_PKG),./...)); do \
	  for target in $$(go test -list '^$(or $(FUZZ_TARGET),Fuzz.*)$$' "$$pkg" | grep '^Fuzz'); do \
	    echo "$$pkg $$target"; \
	    go test -run '^$$' -fuzz "^$$target$$" -fuzztime "$(FUZZTIME)" "$$pkg" || exit 1; \
	  done; \
	done

.PHONY: tidy-modules
tidy-modules: ## Tidy the product module and every tool module
	@for m in $(GO_MODULES); do (cd $$m && go mod tidy) || exit 1; done

.PHONY: verify-modules
verify-modules: ## Check that every module is tidy and matches go.sum
	@for m in $(GO_MODULES); do echo "$$m"; (cd $$m && go mod tidy -diff && go mod verify) || exit 1; done

.PHONY: lint-workflows
lint-workflows: ## Check the GitHub Actions workflows (actionlint)
	$(TOOLS) actionlint

# Without a GitHub token zizmor runs offline. CI passes ZIZMOR_ONLINE=1 and GH_TOKEN to also look
# up known vulnerable actions; GH_TOKEN reaches the container only then.
.PHONY: audit-workflows
audit-workflows: ## Find unsafe patterns in the workflows and Dependabot config (zizmor, Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -w /src $(if $(ZIZMOR_ONLINE),-e GH_TOKEN,) \
	  $(call tool_image,zizmor) $(if $(ZIZMOR_ONLINE),,--offline) .

.PHONY: lint-scripts
lint-scripts: ## Check the shell scripts (ShellCheck, Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -w /src $(call tool_image,shellcheck) \
	  $$(git ls-files '*.sh' .githooks)

# Only the packages linked into the binary: the development tools in tools/ never ship, and some
# of them are GPL. Permissive licenses and MPL-2.0 are compatible with Apache-2.0.
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0,0BSD,Unlicense
.PHONY: check-licenses
check-licenses: ## Check that every package in the binary has an allowed license (go-licenses)
	$(TOOLS) go-licenses check ./... --allowed_licenses=$(ALLOWED_LICENSES) --logtostderr=false --stderrthreshold=ERROR

.PHONY: lint-dockerfiles
lint-dockerfiles: ## Check the Dockerfiles (Hadolint, Docker)
	@for file in $$(git ls-files '*Dockerfile'); do \
	  echo "$$file"; \
	  docker run --rm -i $(call tool_image,hadolint) <"$$file" || exit 1; \
	done

.PHONY: scan-vulnerabilities
scan-vulnerabilities: ## Scan Go dependencies for known vulnerabilities
	go tool govulncheck ./...

.PHONY: check-api-compatibility
check-api-compatibility: ## List API changes that break clients, against BASE (for example BASE=origin/main)
	@test -n "$(BASE)" || { echo "pass BASE=<revision>, for example BASE=origin/main"; exit 1; }
	scripts/api-diff.sh "$(BASE)"

.PHONY: build-images
build-images: ## Build the Docker images without pushing (IMAGE=claude|codex|edge, PLATFORMS=<list>)
	@for image in $(or $(IMAGE),$(IMAGES)); do \
	  echo "image $$image"; \
	  docker buildx build --file docker/$$image/Dockerfile $(if $(PLATFORMS),--platform $(PLATFORMS),) . || exit 1; \
	done

.PHONY: check-cli-releases
check-cli-releases: ## Fail when an agent CLI pin is behind a release older than 14 days (network)
	scripts/check-cli-releases.sh

.PHONY: test-image
test-image: ## Build and smoke-test one agent image without a login (AGENT=claude|codex)
	@test -n "$(AGENT)" || { echo "pass AGENT=claude or AGENT=codex"; exit 1; }
	docker/test-image.sh "$(AGENT)"

# Grype reads each image from a `docker save` archive, so it needs no access to the Docker
# daemon. Its vulnerability database stays in a volume between runs. SARIF=<dir> also writes one
# SARIF file per image.
.PHONY: scan-images
scan-images: ## Scan local images for fixed high or critical vulnerabilities (SCAN="<image> ...", Grype)
	@test -n "$(SCAN)" || { echo "pass SCAN=\"<image> ...\", for example SCAN=native-agent-api-claude:local"; exit 1; }
	@archive=$$(mktemp) && trap 'rm -f "$$archive"' EXIT && failed="" && \
	for image in $(SCAN); do \
	  echo "image $$image"; \
	  name=$$(printf '%s' "$$image" | tr -c 'A-Za-z0-9._-' '_'); \
	  docker save "$$image" -o "$$archive" || exit 1; \
	  docker run --rm -v "$$archive":/scan/image.tar:ro -v native-agent-api-grype:/cache -e GRYPE_DB_CACHE_DIR=/cache \
	    $(if $(SARIF),-v "$(abspath $(SARIF))":/out,) $(call tool_image,grype) docker-archive:/scan/image.tar \
	    --only-fixed --fail-on high -o table $(if $(SARIF),-o sarif=/out/$$name.sarif,) || failed="$$failed $$image"; \
	done; \
	test -z "$$failed" || { echo "vulnerable:$$failed"; exit 1; }

.PHONY: build-release-snapshot
# The image may have an older Go than go.mod; GOTOOLCHAIN=auto fetches the right one into a
# volume, so the next run reuses it.
build-release-snapshot: ## Build the release archives and SBOMs into dist/ without publishing (GoReleaser, Docker)
	docker run --rm -v "$(CURDIR)":/src -w /src -v native-agent-api-goreleaser:/go -e GOCACHE=/go/cache \
	  -e GOTOOLCHAIN=auto -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src \
	  $(call tool_image,goreleaser) release --snapshot --clean --skip=publish

.PHONY: check-release-config
check-release-config: ## Validate .goreleaser.yaml (GoReleaser, Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -w /src $(call tool_image,goreleaser) check

# Three levels. The git hooks run the first two; check-all adds the slow builds that CI runs on
# every pull request. docs/development.md lists which CI job runs which target.
.PHONY: check-pre-commit
check-pre-commit: verify-modules lint-go lint-workflows check-coverage scan-vulnerabilities check-licenses ## Before a commit: Go checks, tests, coverage and licenses (needs only Go)
	@echo "check-pre-commit: OK"

.PHONY: check-pre-push
check-pre-push: check-pre-commit audit-workflows lint-scripts lint-dockerfiles check-api-compatibility check-release-config ## Before a push: adds workflow, script, Dockerfile, API and release checks (needs Docker)
	@echo "check-pre-push: OK"

.PHONY: check-all
check-all: check-pre-push test-fuzz build-images build-release-snapshot ## Everything CI runs that works locally: adds fuzzing, images and release archives
	@echo "check-all: OK"

check-pre-push check-all: BASE ?= origin/main
check-all: FUZZTIME ?= 10s

# Another hooks directory stays: install refuses to replace it, and uninstall removes only ours.
.PHONY: install-git-hooks
install-git-hooks: ## Make git run check-pre-commit before each commit and check-pre-push before each push
	@current=$$(git config --local --get core.hooksPath || true); \
	if [ -n "$$current" ] && [ "$$current" != .githooks ]; then \
	  echo "core.hooksPath is already $$current; remove it first"; exit 1; fi
	git config --local core.hooksPath .githooks
	@echo "git hooks: .githooks (remove with: make uninstall-git-hooks)"

.PHONY: uninstall-git-hooks
uninstall-git-hooks: ## Stop git from running the checks before commits and pushes
	@if [ "$$(git config --local --get core.hooksPath || true)" = .githooks ]; then \
	  git config --local --unset core.hooksPath; echo "git hooks: removed"; \
	else echo "git hooks: not installed by this repository"; fi

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
