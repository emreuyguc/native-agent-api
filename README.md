# native-agent-api

[![CI](https://github.com/emreuyguc/native-agent-api/actions/workflows/ci.yml/badge.svg)](https://github.com/emreuyguc/native-agent-api/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/emreuyguc/native-agent-api/badge)](https://scorecard.dev/viewer/?uri=github.com/emreuyguc/native-agent-api)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**An HTTP API for Claude Code and Codex CLI.** native-agent-api runs the real coding-agent CLI and
serves it over REST, Server-Sent Events and WebSocket. Sessions, login, tools, skills, hooks,
plugins, subagents and MCP servers keep working exactly as in a terminal.

<p align="center">
  <img src="docs/assets/overview.svg" alt="How a run works: 1. your app sends a prompt with an HTTP POST; 2. native-agent-api runs the real Claude Code or Codex CLI; 3. native events stream back over SSE or WebSocket; 4. the result arrives with token usage." width="100%">
</p>

> **Status: 0.x.** The API may still change between minor versions; see the [changelog](CHANGELOG.md).

## Why

The AI providers' own CLIs are excellent agents. Claude Code and Codex CLI manage sessions, run
tools, skills, hooks, plugins and MCP servers, and sign in with your subscription. But they are
built for a person at a terminal on one machine.

Once the CLI runs on a server or another computer, reaching it takes extra tools:

- SSH with `tmux` or `screen`
- remote desktops such as RustDesk, VNC or Chrome Remote Desktop
- browser terminals such as `ttyd`
- agent apps with remote companions, such as Orca or Hermes Desktop

These give a person a screen. They do not give your software an interface.

native-agent-api turns the native CLI into a service:

- **Reach it from anything.** Web apps, mobile apps, bots, scripts and other services talk to the
  agent over plain HTTP, SSE or WebSocket, from any language.
- **Use your subscription, not an API key.** The CLI keeps its own login, so a Claude or ChatGPT
  plan you already have can power your projects and personal tools. Check the provider's terms
  first (see [Notes](#notes)).
- **Native, not re-implemented.** The gateway drives the CLI's own protocol. Every native event
  reaches the client unchanged, and the agent's features keep working.
- **One API for both agents.** Runs, sessions, events, permissions and usage look the same for
  Claude Code and Codex CLI. Agent-only data stays in a typed `provider` section.
- **Your machine, your control.** The gateway runs next to the CLI. No third-party service sits
  between your client and the agent.

## Features

| Area | What you get |
|---|---|
| **Runs** | One-shot asks, or interactive runs you can message, interrupt, close and cancel |
| **Sessions** | Continue with remembered settings, fork, list and delete |
| **Live events** | SSE and WebSocket; reconnect without losing events; optional partial output |
| **Permissions** | Approve or deny each tool use from the client, once or for a session or workspace |
| **Workspaces** | Managed workspaces with a file API, or attached project directories |
| **Native features** | Tools, skills, hooks, plugins, subagents, MCP servers, native control requests |
| **Results and usage** | Structured output, turn and session tokens, context window, cost (Claude Code), subscription allowance |
| **Agent management** | Login without a terminal, logout, status, model list, diagnostics |
| **Security** | Token auth, IP allowlist, DNS-rebinding and cross-origin protection, redacted logs |
| **Operation** | Explicit configuration, generated OpenAPI 3.1, optional hardened Docker setup |

Per-agent details are in the [roadmap and feature status](ROADMAP.md).

## How a run works

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant G as native-agent-api
    participant A as claude / codex
    C->>G: POST /v1/runs/start (prompt, model, workspace)
    G->>A: Start the CLI and send the prompt over stdio
    A-->>G: Native events
    G-->>C: agent events over SSE or WebSocket
    A-->>G: Tool permission request
    G-->>C: permission_request
    C->>G: Allow once, for the session or for the workspace
    G->>A: Native permission answer
    A-->>G: Result and usage
    G-->>C: turn event with result, tokens and cost
    Note over C,A: Later: POST /v1/sessions/{id}/continue or /fork
```

## Supported agents

| Agent | Native CLI | Support |
|---|---|---|
| `claude` | Claude Code | [Support details](docs/agents/claude-code.md) |
| `codex` | Codex CLI | [Support details](docs/agents/codex-cli.md) |

One gateway process serves one agent. Run two processes to serve both. Each has its own login,
port and token, and both speak the same API. Two processes can attach the same directory, so
both agents work on the same files.

<p align="center">
  <img src="docs/assets/two-agents.svg" alt="Two native-agent-api containers, one running Claude Code and one running Codex CLI. Your applications reach both over the same REST, SSE and WebSocket interface, and both agents work in one shared project directory." width="100%">
</p>

With Docker, each installation is one container with its own volumes; see
[several installations](docker/README.md#several-installations).

## Quick start

Requires macOS or Linux, Go, and the agent CLI on `PATH`. Without Go, download a binary from the
[releases](https://github.com/emreuyguc/native-agent-api/releases) and
[verify it](docs/releasing.md#verify-a-release).

```sh
git clone https://github.com/emreuyguc/native-agent-api && cd native-agent-api
go install ./cmd/native-agent-api
cp .env.example .env && chmod 600 .env   # every setting is explicit; review the file
native-agent-api new-token               # client token for $TOKEN, digest for NATIVE_AGENT_TOKEN_SHA256
native-agent-api login claude
native-agent-api serve --env-file .env
```

```sh
curl -s http://127.0.0.1:8080/v1/ask -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"Explain HTTP/2 in one sentence.","model":{"id":"sonnet","effort":"low"}}' | jq -r .result.text
```

`make setup` does these steps interactively: it writes the env file, creates the client token and
starts the gateway on this machine or in Docker.

The interactive API reference is served at `/docs`, and the OpenAPI 3.1 document at
`/openapi.yaml`. For Docker, see [docker/README.md](docker/README.md).

## Documentation

- [API concepts](docs/api.md): runs, sessions, events, permissions, provider sections
- [Configuration](docs/configuration.md): every `NATIVE_AGENT_*` setting
- [Security model](docs/security.md): read this before exposing the gateway
- [Roadmap and feature status](ROADMAP.md): what works per agent, what is planned
- [Platform support](docs/platforms.md), [development](docs/development.md),
  [testing](docs/testing.md), [releasing](docs/releasing.md)

## Notes

native-agent-api is an independent project, not affiliated with Anthropic or OpenAI. It uses the
CLI and the login you provide; your use of each CLI and account stays subject to that provider's
terms, so check them before serving a personal subscription to other people or applications.

Vulnerabilities: see [SECURITY.md](SECURITY.md). Contributions: see [CONTRIBUTING.md](CONTRIBUTING.md). License: [Apache-2.0](LICENSE).
