# Configuration

Configuration is read once at process startup from `NATIVE_AGENT_*` environment variables. Every
`NATIVE_AGENT_*` variable must be present, even when its value is empty; the server has no
configuration defaults or fallbacks. The server has no configuration mutation or reload API. Agent
processes receive an explicit environment instead of inheriting the server environment.

`native-agent-api serve --env-file <path>` reads an explicitly named, owner-only (`0600`)
application env file. The file accepts `NATIVE_AGENT_*` assignments only; it performs no expansion
or command substitution. Process environment values override values from the file. The server never
searches for an env file implicitly.

## Command requirements

| Command | Required configuration |
|---|---|
| `serve` | Every application and native-process variable listed below |
| `login [agent]`, `status [agent]` | `NATIVE_AGENT_AGENT` unless `[agent]` is supplied; `NATIVE_AGENT_DATA_DIR`, `NATIVE_AGENT_PROFILE_DIR`, and `HOME`, `USER`, `PATH`, `TMPDIR` |
| `healthcheck` | `NATIVE_AGENT_LISTEN` only |
| `openapi`, `version`, `new-token`, `help` | No runtime configuration |

`--env-file` belongs to `serve` only. Other commands never search or load an application env file.
They also do not require unrelated server settings such as listener authentication, logging, limits,
or workspace storage.

## Network and access

| Variable | Purpose |
|---|---|
| `NATIVE_AGENT_LISTEN` | Listener in `ip:port` form |
| `NATIVE_AGENT_TOKEN_SHA256` | SHA-256 digest of the bearer token |
| `NATIVE_AGENT_TOKEN_SHA256_FILE` | File containing the token digest |
| `NATIVE_AGENT_TOKEN` | Plain bearer token, minimum 32 characters |
| `NATIVE_AGENT_ALLOW_UNAUTHENTICATED` | `true`: run without a token on any listener. Not combinable with a token |
| `NATIVE_AGENT_ALLOWED_IPS` | Comma-separated source IPs or CIDRs |
| `NATIVE_AGENT_DOCS` | Serve `/docs` and the OpenAPI documents |

Generate a client token and server digest with `native-agent-api new-token`. A non-loopback listener
requires authentication unless `NATIVE_AGENT_ALLOW_UNAUTHENTICATED=true` is explicitly set; on
loopback a token is optional either way.

Without a token the gateway answers only requests addressed to an IP address or `localhost`
(`403 host_not_allowed` otherwise), so a web page cannot reach it by DNS rebinding. In every mode a
state-changing request from another browser origin is rejected (`403 cross_origin_request`).
Clients that are not browsers send no `Origin` header and are not affected. See
[security](security.md).

## Agent and paths

| Variable | Purpose |
|---|---|
| `NATIVE_AGENT_AGENT` | Adapter served by this process |
| `NATIVE_AGENT_SANDBOX_MODE` | `cli` (the CLI's own OS sandbox, fail closed) or `external` (outer boundary such as a container) |
| `NATIVE_AGENT_PROFILE_DIR` | Agent profile: `native` or an absolute directory (see below) |
| `NATIVE_AGENT_DATA_DIR` | Gateway data: `workspaces/` (managed workspaces, attachment and session records) and `work/` (maintenance-command directory) |
| `NATIVE_AGENT_ALLOWED_ROOTS` | Absolute roots allowed for host-directory attachments and extra directories |
| `HOME`, `USER`, `PATH`, `TMPDIR` | Required native CLI process environment |
| `HTTPS_PROXY`, `HTTP_PROXY`, `ALL_PROXY`, `NO_PROXY` (either case) | Optional; passed on to the CLI when set, so it and its tools use the proxy. No other gateway variable reaches the CLI |

Each adapter runs its CLI (`claude`, `codex`) from `PATH`; there is no binary-path setting.

`NATIVE_AGENT_PROFILE_DIR=native` runs the CLI with its own profile under `HOME` (`~/.claude` and
`~/.claude.json`, or `~/.codex`), so an existing login, settings and session history are shared
with the user's own terminal. No location variable is passed to the CLI in this mode; on macOS this
keeps Claude on its default Keychain entry. An absolute path gives the gateway a separate profile,
passed as `CLAUDE_CONFIG_DIR` or `CODEX_HOME`, which needs its own login. The CLI's native
directory is rejected as an absolute path because exporting it changes native behavior.

## Limits

All limit variables shown in `.env.example` are required and validated at startup.

## Logging

All logging variables shown in `.env.example` are required and validated at startup.
`NATIVE_AGENT_LOG_FORMAT` is `json` (collectors, containers), `text` (plain `key=value`) or
`console` (colored, for an interactive terminal; always emits ANSI colors).

Body logging may contain prompts and answers. Known credentials are redacted, but body logging
should remain disabled unless it is needed for diagnosis.
