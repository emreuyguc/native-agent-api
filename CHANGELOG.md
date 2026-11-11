# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/). Before 1.0.0, a minor version may change the API.

## [Unreleased]

### Added

- The API document lists every fixed value: an `enum` for request fields (login `method`,
  `provider` options such as `permission_mode` and `reasoning_summary`, `agent.name`), and the
  values with their meaning in the description of `code`, `error.code` and `capabilities`.
  `docs/api.md` has the same tables, and tests keep both equal to the code.
- Every JSON request body has an example, so clients such as Postman build a minimal valid request
  instead of filling every optional field.

### Changed

- The API document groups operations by task (`login`, `ask`, `permissions`, `native control`
  are new groups) and names each one in a few words, such as "Start login". The former one-line
  summary starts the description. Operation IDs and paths are unchanged.

- Docker: one Compose file serves one installation. A Docker env file per installation holds the
  Compose and container settings and points to the application env file with
  `CONTAINER_APP_ENV_FILE`. `CONTAINER_AGENT` selects the image, and `COMPOSE_FILE` adds the
  restricted egress overlay. The `compose.<agent>.yaml` overlays, the profiles and the shared
  `docker/.env` are removed. `docker/verify.sh` takes the Docker env file. Volume names stay; see
  the upgrade steps in `docker/README.md`.

### Security

- Go 1.27.4, which fixes GO-2026-6859 in `net/http`. The Docker build image is pinned to it.

## [0.1.0] - 2026-11-07

First public release.

### Agents

- Claude Code adapter over `stream-json` (one process per run, native control protocol).
- Codex adapter over `codex app-server` v2.
- One gateway process serves one agent, selected with `NATIVE_AGENT_AGENT`.

### API

- `POST /v1/ask` and `/v1/ask/stream`: one question, with a workspace (tools, persisted session)
  or without one (no tools, nothing kept).
- Runs: start, follow over SSE (resumable with `Last-Event-ID`) or WebSocket, send messages,
  interrupt, close, cancel; finished runs are retained for a configurable time.
- Sessions: continue with only a prompt (settings are remembered), fork, list, delete metadata.
  Native transcripts stay owned by the CLI.
- Workspaces: managed directories or attached host directories under `NATIVE_AGENT_ALLOWED_ROOTS`,
  with a file API (upload, ranged download, copy, move, mkdir, delete).
- Tool permissions: `bypass` or `ask`; requests have a common `kind` (`command`, `file_change`,
  `mcp`, `other`) and summary, and allows can be scoped to one request, a session or a workspace.
- Native control requests in both directions, task/subagent/process observations, partial output.
- Common result fields for every agent: `text`, `outcome`, `error`, `tokens` (all model calls of
  the turn, uncached input), `context {window, used}`; run and session usage totals.
- Typed `provider` sections in requests and responses for agent-only data, next to the unmodified
  native document; Claude reports turn and conversation cost.
- Agent endpoints: description, models with reasoning efforts, login (no terminal needed), logout,
  status, subscription usage, diagnostics.
- Generated OpenAPI 3.1 document and reference at `/docs`.

### Security

- Every setting is explicit; there are no configuration defaults.
- Bearer token (plain or SHA-256 digest), required beyond loopback unless explicitly disabled;
  IP allowlist; DNS-rebinding and cross-origin request protection.
- Agent processes get an explicit environment, never the server's; logs are redacted.
- `cli` sandbox mode for both agents, and protection against agent-written MCP, hook and git
  configuration.
- Optional hardened Docker setup per agent, with resource limits and an egress-controlled variant.

[Unreleased]: https://github.com/emreuyguc/native-agent-api/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/emreuyguc/native-agent-api/releases/tag/v0.1.0
