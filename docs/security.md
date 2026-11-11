# Security model

The gateway lets an HTTP client run a coding agent that executes commands and changes files. This
page states what that exposes, who is trusted, where the boundaries are and which tests guard
them. Read it before you expose a gateway; use it to judge whether a report crosses a boundary
([SECURITY.md](../SECURITY.md)).

## Principal

One gateway serves **one principal**: whoever holds the bearer token. There are no users or roles;
every authenticated client can use every workspace, session and run. Clients that do not trust each
other need separate gateways.

## Assets

| Asset | Where | Risk |
|---|---|---|
| Agent login | CLI profile (`~/.claude`, `~/.codex`, macOS Keychain) | Account takeover, spent subscription |
| Gateway token | Client; the server keeps its SHA-256 | Full control, which means code execution |
| Workspace files | `<data>/workspaces`, allowed roots | Owner's code and data |
| Session state | `<data>/workspaces/.sessions` | Native session bindings, remembered settings |
| Host or container | Where the gateway runs | The agent runs commands there |
| Prompts, answers, events | Memory, logs | May contain workspace secrets |

## Actors

| Actor | Trust |
|---|---|
| Owner, operator | Trusted |
| Client with the token | Trusted; the content it sends is not |
| Network peer without the token | Hostile |
| Web page in the owner's browser | Hostile (cross-site requests, DNS rebinding) |
| Agent process | **Semi-trusted:** it reads untrusted text (repositories, web pages, uploads, MCP results), so prompt injection can control it |
| Workspace content | Hostile |
| Plugins, MCP servers, hooks | As trusted as their source; chosen by the client |
| Native CLI binary | Trusted (pinned version and checksum in Docker) |

## Boundaries

1. **Network → gateway:** token, IP allowlist, listener rules, request limits.
2. **Browser → gateway:** Host and Origin checks; matters most on loopback without a token.
3. **Gateway → agent:** validated arguments, an environment built from scratch (only the proxy
   variables pass through, see [configuration](configuration.md#agent-and-paths)), process groups,
   timeouts, output limits.
4. **Agent → host:** the real boundary. On a host it is the CLI's own sandbox
   (`NATIVE_AGENT_SANDBOX_MODE=cli`); in Docker it is the container (`external`). The gateway adds
   none of its own.
5. **Client paths → filesystem:** no traversal, symlinks or hardlinked files; gateway directories
   are protected.

## Controls

| Area | Controls |
|---|---|
| Authentication | Token SHA-256 compared in constant time; at least 32 characters; required beyond loopback; only health and docs are public; failures are logged |
| Network | Allowlist by TCP source address (forwarding headers ignored); explicit `ip:port` listener; without a token only IP or `localhost` Host names |
| Browser | No CORS headers; cross-origin state changes rejected (Origin, `null`, Sec-Fetch-Site); JSON bodies need `application/json`; same-origin WebSocket only; docs page with strict CSP and SRI |
| Request limits | Body, header and WebSocket message size; header timeout; run slots; event, observation and retention limits; timeouts. A reader that accepts no bytes for a minute is disconnected |
| Child processes | No shell; arguments from validated values; own environment and process group; cancel kills the whole tree, tool groups included; output and stderr limits |
| Filesystem | Paths reject absolute, `..`, NUL, backslash and symlinks; every operation runs on an `os.Root`; only single-link regular files are opened, without blocking; atomic uploads; entry limits on delete and copy |
| Native options | Strict per-adapter schemas; gateway-owned keys (sandbox, approval, config paths) rejected. Plugins, MCP, hooks and native controls act with the client's authority, which already includes starting commands. The agent cannot call them: the CLI sandbox blocks loopback to the gateway |
| Secrets in output | Tokens redacted in errors and debug output; body logging off by default, never for file transfers or login |
| Supply chain | Binaries and images built only by the release workflow from a version tag after the checks; signed build provenance (Sigstore), SBOM and checksums; pinned actions and base images. Workflows checked by actionlint and zizmor; pull requests cannot add a dependency with a known high vulnerability; OpenSSF Scorecard weekly |
| Container | Non-root UID, read-only root filesystem, no capabilities, `no-new-privileges`, PID limit, bounded tmpfs, pinned CLI checksums. The container gets only the application env file, never the Docker env file (`docker/verify.sh` checks it). Optional egress overlay: no route or external DNS, only allowlisted hosts through a proxy |

## Protections with regression tests

Each row is a concrete attack and the test that keeps it closed. Tests marked *host* need the real
CLI: `make test-host AGENT=claude|codex`.

| ID | Attack | Protection | Test |
|---|---|---|---|
| F-1 | DNS rebinding: without a token on loopback, a web page starts a `bypass` run | Without a token, only IP and `localhost` Host names are served | `TestBrowserGuard`, `FuzzLiteralHost` |
| F-2 | Cross-site form post: a web page logs out, starts a login, creates a workspace or cancels a run | Cross-origin state changes are rejected in every mode | `TestBrowserGuard`, `FuzzCrossOrigin` |
| F-3 | Flag injection: `allowed_tools: ["--settings={...}"]` becomes a Claude flag and overrides the sandbox | Tool rules never start with `-` | `TestParseOptionsRejectsUnsafeOrInvalidValues` |
| F-4 | A FIFO in the workspace makes download or copy hang forever | Files open with `O_NONBLOCK`; only single-link regular files are served | `TestSpecialFilesNeverBlock` |
| F-5 | A parent swapped for a symlink between check and use escapes the workspace | Every operation runs on an `os.Root` of the workspace | `TestSwappedParentCannotEscape` |
| F-6 | A control character inside a token (`Bear\x00er …`) slips past redaction | Control characters are stripped before redaction | `FuzzMessage` |
| F-7 | Under `cli`, file tools run outside the sandbox; the agent writes an MCP server to `.mcp.json` or `.codex/config.toml`, and the next run starts it unconfined | Claude: `Edit` deny rules for `.mcp.json`, `.claude/**`, `.git/**` in the workspace and add_dirs. Codex: threads open without `cwd`, so project config, hooks and exec policies never load (`AGENTS.md` still does) | `TestAgentWrittenConfigNeverStarts` (host) |
| F-8 | Codex records every workspace as trusted in `~/.codex/config.toml`, which lets project config load (F-7) | Threads open without `cwd`; the native profile stays unchanged | Conformance fake rejects `cwd`; `TestAgentWrittenConfigNeverStarts` (host) |

## Decisions

| ID | Decision | Reason and consequence |
|---|---|---|
| D-1 | Gateway and agent share one OS user | Separate users need `CAP_SETUID`, which turns a gateway compromise into root in the container, for little gain: the agent reads its own login anyway. See the limits below |
| D-2 | Loopback may run without a token | For local use. F-1 and F-2 stop web pages; other processes of the same user already are that user; a sandboxed agent cannot connect to `127.0.0.1` |
| D-3 | Egress is open by default; a restricted overlay is optional | Web tools, MCP and package installs need it. `docker/compose.egress.yaml` allows only the provider API and listed hosts |

## Known limits

- **The agent can read its login.** The CLI needs it, so a prompt-injected agent can leak it
  unless the CLI sandbox stops it. In Docker nothing does.
- **Same OS user (D-1).** A compromised agent can read and change gateway files, read a plain
  `NATIVE_AGENT_TOKEN` from the env file and stop the gateway. Where the agent runs unconfined,
  store only the digest (`NATIVE_AGENT_TOKEN_SHA256`).
- **Detached processes.** A process the agent detaches on purpose (`nohup cmd &`) survives cancel.
  It stays inside the CLI sandbox or the container.
- **No rate limit.** A 32+ character token cannot be guessed, and every request beyond the headers
  needs it.
- **Egress overlay hides client addresses.** Every client appears as the forwarder, so restrict
  access with the host port binding and firewall, not `NATIVE_AGENT_ALLOWED_IPS`.
- **F-9: Codex `workspace-write` can write `/tmp` and `$TMPDIR`.** This is native behavior, so
  concurrent runs can touch each other's temp files. The gateway never puts its own per-run files
  where the agent's sandbox can write.
