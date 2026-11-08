# Codex CLI adapter

The adapter uses the Codex CLI v2 app-server protocol over stdio, one app-server process per run.
The tested CLI version is `cli.tested_version` in `GET /v1/agent`. App-server is used because it
has persistent threads, streamed events, interrupts and approval responses; `codex exec --json` is
meant for standalone automation. The contract sources are the schema from
`codex app-server generate-json-schema`, the CLI help and the official app-server documentation.

## Native surface

| Gateway concept | Codex surface | Policy |
|---|---|---|
| Version, diagnostics | `codex --version`, `codex doctor --json` | Bounded native output |
| Login | `account/login/start`, `account/login/completed`, `codex login status`, `codex logout` | `chatgpt_device_code` (works from any machine) or `chatgpt` (browser on the gateway host; the native callback listens on localhost) |
| Models | `model/list` | Native catalog; not an entitlement guarantee |
| Subscription usage | `account/rateLimits/read`, `account/usage/read` | Separate from run tokens |
| New session | `thread/start` | The native thread ID is stored as the session's binding |
| Resume, fork | `thread/resume`, `thread/fork` | Needs the stored binding; no "last session" fallback |
| Turns | `turn/start`, `turn/interrupt`, `turn/started`, `turn/completed` | Common result and status semantics |
| Streaming | App-server notifications | Every native message is kept unchanged |
| Approvals | Command and file approval requests | Kinds `command` and `file_change`; the method and native request stay in `provider` |
| Tasks, processes | `item/started`, `item/completed`, background-terminal methods | Observations, with the native events kept |
| Skills, hooks, MCP | `skills/list`, `hooks/list`, `mcpServerStatus/list` | Native data and controls |
| Cancellation | `turn/interrupt`, process stop | Interrupt keeps the thread; cancel stops the app-server |

Codex-only configuration, plugins, reviews, projects, realtime audio, remote control, marketplace
and experimental methods stay native: use the native control endpoint.

## Token usage

`thread/tokenUsage/updated` arrives once per model call.

- `result.tokens` is the sum of `last` over every model call of the turn. Native `inputTokens`
  include cached and cache-write input; the gateway subtracts them, so `input` is uncached input as
  for Claude.
- `result.context` is the last call's `inputTokens` against `modelContextWindow`.
- `provider.thread` holds the thread totals. They carry over when a session is continued; a fork
  starts from its parent's.

## Request policy

- Model and reasoning effort are required.
- `tools.permissions`: `ask` → `on-request`, `bypass` → `never`.
- `NATIVE_AGENT_SANDBOX_MODE=cli`: workspace runs use the `workspace-write` sandbox,
  `tools.add_dirs` become `writableRoots`, and network access stays off. `external`: threads use
  `danger-full-access` and turns the `externalSandbox` policy, so the container enforces isolation
  and network policy.
- Threads open without an explicit `cwd`. The app-server already runs in the working directory, and
  an explicit `cwd` makes `thread/start` record the project as trusted in the native profile. The
  workspace stays an undecided project: `AGENTS.md` is loaded, but project config
  (`.codex/config.toml`), hooks and exec policies are not, so an MCP server the agent writes there
  never starts ([security](../security.md) F-7, F-8). A directory the owner marked trusted loads its
  project config as in Codex itself; do not mark attached directories trusted when nothing but the
  CLI sandbox confines the agent.
- Incremental notifications (message, reasoning and plan deltas, command and process output, tool
  progress) are live-only `partial` events, received with `?partial=true`; the complete item arrives
  in `item/completed`. Kept, a long command output alone would fill the run's event limit (about one
  notification per output line).
- App-server has no turn or output-token limit, so those Claude options are not in the Codex
  `provider` schema.

An ask without a workspace is confined, because the app-server cannot remove every built-in tool:
an `ephemeral` thread (nothing persisted) in a private throwaway directory, the `read-only` sandbox
with `readOnly` turns and no network, approval policy `never`, and no MCP servers. This holds in both
sandbox modes, so any command attempt fails closed. Only the `developer_instructions`,
`service_tier` and `reasoning_summary` options are accepted there.

## Provider options

The run `provider` section is `"name": "codex"` plus these options; unknown fields are rejected.

| Option | Native mapping |
|---|---|
| `config` | Thread `config` overrides (`mcp_servers`, `features`, `tools`, `web_search`, hooks, ...) |
| `developer_instructions` | Thread `developerInstructions` |
| `approvals_reviewer` | Thread `approvalsReviewer` (`user` or `auto_review`); needs `permissions=ask` |
| `service_tier` | Thread `serviceTier` |
| `reasoning_summary` | Turn `summary` (`auto`, `concise`, `detailed`, `none`) |
| `disabled_plugin_ids` | Turn `disabledPluginIds`; an empty list clears it |
| `skill_roots` | `skills/extraRoots/set` for the run's app-server; entries must be inside an allowed root or the workspace |

`config` keys that gateway policy or explicit request fields own are rejected, dotted sub-keys
included: approval, sandbox, permission profiles, profiles, projects, model and provider selection,
instructions, login and credential storage, and backend base URLs.

Official guidance: [Codex as a platform](https://developers.openai.com/blog/codex-as-a-platform),
[Codex app-server](https://learn.chatgpt.com/docs/app-server).
