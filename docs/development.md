# Development

Requirements: Go (version in `go.mod`) and, for the full checks, Docker. Every other tool comes
with the repository at a fixed version.

```sh
make help
make check-pre-commit
make build
make run
make format-code
```

`make run` builds and serves with `--env-file .env` (git-ignored; copy `.env.example`);
pass `ENV_FILE=path` for another file. Tests that need the real CLIs are described in
[testing](testing.md).

## Checks

Every check is a `make` target, and CI runs the same checks with the same tool versions. Where
GitHub does it faster, CI uses its own tools: the released golangci-lint binary and a BuildKit
cache for the images.

| Target | When | What it runs | Time (warm) |
|---|---|---|---|
| `make check-pre-commit` | Before a commit; needs only Go | `verify-modules`, `lint-go`, `lint-workflows`, `check-coverage` (runs `test`), `scan-vulnerabilities`, `check-licenses` | Seconds when little changed; about 50 s from scratch |
| `make check-pre-push` | Before a push; needs Docker | `check-pre-commit`, then `audit-workflows`, `lint-scripts`, `lint-dockerfiles`, `check-api-compatibility`, `check-release-config` | About 30 s more |
| `make check-all` | Before a release or a large change | `check-pre-push`, then `test-fuzz`, `build-images`, `build-release-snapshot` | Some minutes |

| Target | Checks | CI job |
|---|---|---|
| `verify-modules`, `lint-go`, `lint-workflows`, `check-coverage`, `scan-vulnerabilities`, `check-licenses`, `lint-scripts`, `lint-dockerfiles`, `check-release-config` | Modules, Go lint, workflow lint, tests and coverage, known vulnerabilities, licenses of the linked packages, shell scripts, Dockerfiles (Hadolint), `.goreleaser.yaml` | `ci` / `check` |
| `test` | The tests on macOS, for the darwin binaries | `ci` / `macos` |
| `audit-workflows` | Workflow security (zizmor) | `ci` / `workflows` |
| `check-api-compatibility BASE=…` | API changes that break clients | `api` / `breaking-changes` |
| `test-fuzz FUZZTIME=…` | New fuzz inputs; `FUZZ_PKG=` and `FUZZ_TARGET=` pick one target; `check-all` uses 10 seconds per target | `fuzz`, weekly, one job of 10 minutes per target |
| `check-cli-releases` | Each agent CLI pin is at most 14 days behind its releases | `cli-releases`, weekly |
| `build-images` | The Docker images, without pushing | `ci` / `docker` (amd64 and arm64) |
| `test-image AGENT=…` | One agent image starts hardened and healthy without a login (`docker/verify.sh`) | `ci` / `docker` (amd64) |
| `scan-images SCAN=…` | Local images have no fixed high or critical vulnerability (Grype) | `ci` / `docker`; `scan`, weekly over the latest release |
| `build-release-snapshot` | The release archives and SBOMs in `dist/`, without publishing | `release` publishes them |

`make test` reuses the results of packages that did not change; `TESTFLAGS=-count=1` runs every
test. CI also passes `-shuffle=on`, which runs the tests in random order. `check-pre-push` and
`check-all` compare the API with `origin/main`; pass `BASE=` for another base.

`lint-go` also enforces two project rules from `AGENTS.md`:

| Linter | Rule |
|---|---|
| `depguard` | Only `internal/agent/builtin` imports an adapter package |
| `forbidigo` | Only `internal/cli` reads the process environment |

`check-coverage` checks the total and, for the packages that guard a security boundary, a limit
per package (`.testcoverage.yml`).

`make install-git-hooks` makes git run `check-pre-commit` before each commit and `check-pre-push`
before each push (`.githooks/`); `make uninstall-git-hooks` stops it. The commit hook refuses
unstaged changes, and the push hook refuses to push a commit other than the checked-out one: the
checks read the working tree. Skip a hook once with `--no-verify`.

Only GitHub can run these: the dependency review, CodeQL, OpenSSF Scorecard, the code scanning
upload of the weekly image scan, and the signed
provenance and publishing of a release.

### Tool versions

| Tools | Version pinned in | Runs with |
|---|---|---|
| actionlint, oasdiff, go-test-coverage, go-licenses | `tools/go.mod` | `go tool` |
| golangci-lint | `tools/golangci-lint/go.mod` | `go tool`; its own module because of its dependencies |
| govulncheck | `go.mod` | `go tool` |
| zizmor, ShellCheck, Hadolint, Grype, GoReleaser | `tools/docker/<name>/Dockerfile` (tag and digest) | Docker; the release workflow runs the same GoReleaser version |

None of them changes the product's dependencies. Dependabot updates all of them.

### Agent CLI versions

Dependabot does not know the agent CLIs. The weekly `cli-releases` workflow fails when a pin is
behind a release older than 14 days. To update one:

1. In `docker/<agent>/Dockerfile`, set `cli_version` and the SHA-256 of each platform download.
2. Set `TestedVersion` in the adapter's `Descriptor` and the agent row of `README.md`; a test checks
   that all three agree.
3. Run the real CLI checks of [testing](testing.md).

## API compatibility

`make check-api-compatibility BASE=origin/main` compares the generated OpenAPI document with the one of `BASE`.
It lists every change that can break an existing client, such as a removed field or a new
`enum`. A pull request runs it against its base branch and fails on such a change. When the change
is intended, add the `api-breaking` label to the pull request and describe the change in
`CHANGELOG.md`.

## Repository layout

```text
cmd/native-agent-api/   process entry point
internal/cli/           commands and server lifecycle
internal/config/        startup configuration and validation
internal/httpapi/       HTTP, SSE, WebSocket, OpenAPI, and middleware
internal/agent/         adapter contract and native CLI adapters
internal/run/           runs, events, retention, and timeouts
internal/session/       persisted session metadata and run locks
internal/workspace/     workspaces, host-directory attachments, and file operations
internal/permission/    in-memory permission rules
internal/fsroot/        allowed-root policy
docker/                optional container images and Compose definitions
scripts/               interactive setup, release notes, API diff and the end-to-end acceptance runner
```

A new adapter goes in `internal/agent/<name>`, is registered in `internal/agent/builtin`, and gets
a support document in `docs/agents/` and a Docker definition in `docker/<name>/`.

The server uses standard `net/http` with Huma for operations and generated OpenAPI 3.1. SSE is the
resumable server-to-client stream; WebSocket adds bidirectional controls.
