# Development

Requirements: Go (version in `go.mod`) and, for the full checks, Docker. Every other tool comes
with the repository at a fixed version.

```sh
make help
make check-pre-commit
make build
make run
make fmt
```

`make run` builds and serves with `--env-file .env` (git-ignored; copy `.env.example`);
pass `ENV_FILE=path` for another file. Tests that need the real CLIs are described in
[testing](testing.md).

## Checks

Every check is a `make` target. CI calls the same targets, so a check that passes locally passes
in CI, and the checks move with the repository to another CI service.

| Target | When | What it runs | CI job |
|---|---|---|---|
| `make check-pre-commit` | Before a commit | `modules`, `lint`, `lint-workflows`, `coverage` (runs `test`), `vuln` | `ci` / `check` |
| `make check-pre-push` | Before a push; needs Docker | `check-pre-commit`, then `lint-zizmor`, `lint-shell`, `api-diff`, `fuzz`, `images`, `snapshot` | All jobs below |
| `make lint-zizmor` | | Workflow security (zizmor) | `ci` / `workflows` |
| `make lint-shell` | | Shell scripts (ShellCheck) | `ci` / `check` |
| `make api-diff BASE=…` | | API changes that break clients | `api` / `breaking-changes` |
| `make fuzz FUZZTIME=…` | | New fuzz inputs; `check-pre-push` uses 10 seconds per target | `fuzz`, weekly with 2 minutes |
| `make images` | | The Docker images, without pushing | `ci` / `docker` (amd64 and arm64) |
| `make snapshot` | | The release archives in `dist/`, without publishing | `release` publishes them |

`check-pre-push` compares the API with `origin/main`; pass `BASE=` for another base.
`make hooks` makes git run `check-pre-commit` before each commit and `check-pre-push` before each
push (`.githooks/`). Skip a hook once with `--no-verify`.

Only GitHub can run these: the dependency review, CodeQL, OpenSSF Scorecard, and the signed
provenance and publishing of a release.

### Tool versions

| Tools | Version pinned in | Runs with |
|---|---|---|
| actionlint, oasdiff, go-test-coverage | `tools/go.mod` | `go tool` |
| golangci-lint, GoReleaser | `tools/golangci-lint/go.mod`, `tools/goreleaser/go.mod` | `go tool`; own modules because of their dependencies |
| govulncheck | `go.mod` | `go tool` |
| zizmor, ShellCheck | `tools/docker/<name>/Dockerfile` (tag and digest) | Docker |

None of them changes the product's dependencies. Dependabot updates all of them.

## API compatibility

`make api-diff BASE=origin/main` compares the generated OpenAPI document with the one of `BASE`.
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
