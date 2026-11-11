# Development

Requirements: Go 1.27.4+, golangci-lint 2.15, and Docker for the container images.

```sh
make help
make check
make build
make run
make fmt
```

`make check` runs module verification, linting, race-enabled tests, and `govulncheck`.
`make run` builds and serves with `--env-file .env` (git-ignored; copy `.env.example`);
pass `ENV_FILE=path` for another file. Tests that need the real CLIs are described in
[testing](testing.md).

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
```

A new adapter goes in `internal/agent/<name>`, is registered in `internal/agent/builtin`, and gets
a support document in `docs/agents/` and a Docker definition in `docker/<name>/`.

The server uses standard `net/http` with Huma for operations and generated OpenAPI 3.1. SSE is the
resumable server-to-client stream; WebSocket adds bidirectional controls.
