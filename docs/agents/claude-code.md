# Claude Code adapter

The adapter runs `claude` from `PATH` in headless mode: one process per run, `stream-json` over
stdin/stdout, and the native login. Terminal UI, desktop, IDE, cloud sessions, updates and
installation are not part of the API. `GET /v1/agent` lists the implemented capabilities; this page
explains behavior and policy.

## Login, models and diagnostics

- **Login** methods are `claudeai` (subscription) and `console` (API billing), flow `paste_code`:
  the CLI prints a sign-in URL, the page shows a code, and the code is sent to the CLI's stdin. No
  terminal is needed, so it works in containers. The login `provider` section's `email` and `sso`
  map to the native flags. Logout runs `claude auth logout`.
- **Models** come from the native `initialize` control request, sent to a process without tools,
  hooks or a prompt (no tokens are used). Model IDs are the native `--model` values, aliases such as
  `opus` included. Without a login the CLI reports only its base aliases.
- **Diagnostics** run the read-only `claude doctor` and return its bounded text report.

## Request mapping

| API field | Native behavior |
|---|---|
| `prompt` | User message over stdin |
| `model.id`, `model.effort` | `--model`, `--effort` |
| `instructions.system_prompt` | Temporary `--system-prompt-file` |
| `output.schema` | `--json-schema` |
| `?partial=true` on a stream | Always `--include-partial-messages`; the reader chooses whether to receive them |
| `tools.permissions` | Bypass, or permission prompts answered by the client |
| `tools.add_dirs` | Validated `--add-dir` values |
| start / continue / fork | `--session-id`, `--resume`, `--resume --fork-session`, with gateway-managed session IDs |
| ask without workspace | `--tools ""`, empty strict MCP config, no setting sources, `--no-session-persistence` |
| `provider.max_turns` | `--max-turns` (1–1000) |
| `provider.max_output_tokens` | `CLAUDE_CODE_MAX_OUTPUT_TOKENS` (256–128000) |
| `provider.system_prompt_snapshot` | `--system-prompt-snapshot`: `on` or `off` |
| `provider.file_checkpointing` | Native SDK file checkpoints for workspace runs: `on` or `off` |
| `provider.permission_mode` | `--permission-mode` with `tools.permissions: ask`: `acceptEdits`, `auto`, `dontAsk`, `manual` or `plan` |
| `provider.debug`, `provider.debug_filter` | Bounded, redacted `diagnostic` events from stderr; the filter is the native debug category filter |

The run `provider` section is `"name": "claude"` plus these options. Further options map the native
flags for agents, tools, allowed and disallowed tools, permission mode, budget, fallback model,
session name, hook events, subagent output, compaction, slash commands, safe and bare modes, beta
flags, plugins, files, MCP configuration, settings and the `initialize` request. Every option is
optional; an omitted one is not passed, so the CLI keeps its native behavior. Unknown fields are
rejected.

## Results

| Field | Source |
|---|---|
| `result.tokens` | Native `usage`: all model calls of the turn |
| `result.context` | Input of the main agent's last model call (input + cache read + cache creation; subagent calls do not count) against that model's `contextWindow` |
| `result.outcome` | `success` → `completed`, `error_max_*` → `limit_reached`, a result after an interrupt → `interrupted`, otherwise `failed` |
| `provider.reason` | Native `subtype` |
| `provider.turn {cost_usd, models}` | Native `model_usage_since_last_result`: this turn, also the first turn of a continued session |
| `provider.session {cost_usd, models}` | Native `total_cost_usd` and `modelUsage`: the whole conversation. Carried over when a session is continued; a fork starts from its parent's |
| `provider.native` | The unmodified native `result` event |

A `result` whose `origin.kind` is set and is not `human` (for example `task-notification`) ends a
turn the CLI started itself for a background task. It is forwarded as an `agent` event, not as a
gateway `turn`. A resumed session can report such a turn, for a task the previous process left
behind, before it answers the new prompt.

## Permissions, control and tasks

- A `can_use_tool` request becomes a permission request. Kinds: `Bash` → `command`; `Write`,
  `Edit`, `MultiEdit`, `NotebookEdit` → `file_change`; `mcp__*` → `mcp`; anything else → `other`.
  The summary is the command, file path, URL or native description; the tool name is
  `provider.tool`. An allow can replace the tool input with `provider.updated_input` (for example
  the answers of `AskUserQuestion`).
- Every other native control request is forwarded with its `subtype` in `provider.subtype`; there is
  no fixed list. This covers model and permission changes, context and settings queries, plugin
  reloads, MCP operations and task control.
- With `provider.file_checkpointing: "on"`, replayed user events carry checkpoint UUIDs. Send
  `{"subtype":"rewind_files","user_message_id":"<uuid>"}` to the run control endpoint to restore
  tracked files; conversation history is not rewound.
- `task_started`, `task_updated` (with a status change) and `task_notification` become
  observations: a `subagent` when the task has a subagent type, a `process` for a shell task,
  otherwise a `task`.

## Sandbox and fixed policy

`NATIVE_AGENT_SANDBOX_MODE` is applied over `provider.settings`. `cli` forces
`sandbox.enabled=true` and `sandbox.failIfUnavailable=true`; `external` forces
`sandbox.enabled=false`. Other `sandbox` keys from the client are kept.

Under `cli` the gateway also adds `Edit` deny rules for `.mcp.json`, `.claude/**` and `.git/**` in
the workspace and every `tools.add_dirs` entry, after the client's own rules. The OS sandbox already
stops Bash from writing these files, but the file tools run outside it; without the rules a
prompt-injected agent could write an MCP server or hook that the next run starts unconfined.
Existing files keep working; the agent only cannot create or change them.

- Workspace runs keep native tools, skills, slash commands, hooks, MCP servers, plugins,
  subagents, project instructions, auto-memory and persisted sessions.
- Asks without a workspace disable tools, MCP, setting sources and session persistence.
- File tools stay inside the working directories; browser integration and automatic updates are
  off; the CLI does not inherit the server environment.
- `CLAUDE_CODE_SKIP_PROMPT_HISTORY` is not set, because it breaks session resume.
