# API concepts

The generated OpenAPI document is the complete contract for routes, fields, responses and
validation: see `/docs`, `/openapi.yaml` or `native-agent-api openapi`. Reads are `GET`; every
change is a `POST` to a path that names the action (`…/delete`, `…/cancel`). `GET /v1/agent`
reports the served CLI, its version, the gateway build and [capabilities](#capabilities).

Every fixed value of a field is listed on this page or, for a `provider` section, in the agent's
[support document](agents/). The OpenAPI document lists the same values: an `enum` for request
fields, a list in the description for response fields that can grow. A client must accept a
response value it does not know.

## Capabilities

`capabilities` in `GET /v1/agent` lists the native surfaces the served adapter implements.
Gateway-wide features (REST, streaming, files, workspaces) are not listed.

| Capability | Meaning |
|---|---|
| `auth` | Native login, login status and logout through the API |
| `custom_agents` | Subagents defined per run |
| `debug_output` | Native debug output on request |
| `diagnostics` | GET /v1/agent/diagnostics |
| `file_checkpointing` | Native file checkpoints for workspace runs |
| `file_inputs` | Provider files downloaded into the workspace at run start |
| `hooks` | Native hooks |
| `interactive` | Runs that accept further messages (POST /v1/runs) |
| `mcp` | MCP servers |
| `models` | GET /v1/agent/models |
| `native_control` | Native control requests in both directions |
| `partial_messages` | Partial output streamed as live chunks |
| `permissions` | Tool permission requests (tools.permissions: ask) |
| `plugins` | Native plugins |
| `session_fork` | Forking a session |
| `sessions` | Persisted sessions that later runs continue |
| `skills` | Native skills |
| `subscription_usage` | GET /v1/agent/usage |
| `system_prompt_snapshot` | Native system prompt snapshots |
| `tools` | Native tools in a workspace |

## Runs and asks

A run owns one native CLI process and has one or more turns. Only one run can use a session at a
time.

- `POST /v1/runs/start` creates a session in a workspace and starts its first run. The run
  survives client disconnects and accepts more messages until it is closed, cancelled or idle.
- `POST /v1/sessions/{session_id}/continue` starts the session's next run. Only `prompt` is
  required: the remembered settings are reused, and supplied fields replace them.
- `POST /v1/ask` waits for the answer to one prompt; `POST /v1/ask/stream` streams its events as
  SSE. Disconnecting cancels the run. With `workspace_id` the ask uses the native features of that
  directory and returns a `session_id` that can be continued. Without it, the ask runs without
  tools, MCP servers or settings in a private throwaway directory, and nothing is kept.

## Settings

Run settings are grouped. An omitted optional setting keeps the agent's native behavior; the
gateway never substitutes its own value.

```json
{
  "workspace_id": "…",
  "prompt": "Fix the failing test",
  "model": {"id": "sonnet", "effort": "high"},
  "tools": {"permissions": "ask", "add_dirs": ["/projects/shared"]},
  "instructions": {"system_prompt": "…"},
  "output": {"schema": {"type": "object"}},
  "provider": {"name": "claude", "max_turns": 8}
}
```

`model` is always required. `tools` is required with a workspace and not allowed without one.
`provider` holds the served agent's own options ([agents](agents/)).

`tools.permissions` is `bypass` (tools run without asking) or `ask` (each tool use waits for an
answer, see [Permissions](#permissions)).

## Provider sections

Every field outside a `provider` object is gateway data, or data every agent supplies with the same
meaning, and is always present. What only one agent has is in a `provider` object, in requests and
responses:

```json
"provider": {"name": "claude", "tool": "Bash", "native": {"…": "…"}}
```

- `name` is the served agent and selects the schema variant. The OpenAPI document describes each
  section as a `oneOf` with one variant per agent, whichever agent is served.
- Request sections (run settings, login start, permission allow) hold only the agent's own fields.
  A section naming another agent is rejected with 422 `provider_mismatch`, so a client never
  believes an ignored setting took effect.
- Response sections (auth status, subscription usage, models, turn results, permission and control
  requests) add the agent's typed fields and `native`, the unmodified native document. Data a CLI
  reports only partly is never promoted to a common field.

## Response shapes

Responses group fields by what they describe. Lists are always arrays, never `null`. A run snapshot:

| Group | Contents |
|---|---|
| `run_id`, `status` | `running`, `succeeded`, `failed`, `cancelled` |
| `context` | `workspace_id`, `session_id`, `native_session_id`; empty for a stateless ask |
| `mode` | `interactive`, `stateless` (ask without a workspace), `permissions` |
| `progress` | `turns`, `events`, `input_closed` |
| `waiting` | `permissions` and `controls` to answer, each with `times {created_at}` |
| `observations` | `items` (tasks, subagents, processes) and `dropped` |
| `result` | Last turn, see below |
| `usage` | Token totals of the run: `turns {reported, unreported}`, `tokens`, and `partial` (counts some reported turn left out) |
| `error` | `code` and `message` of a failed run (codes below) |
| `times` | `created_at`, `started_at`, `updated_at`, `finished_at` |
| `trace` | `request_id` of the request that started the run |

A failed or cancelled run has one of these `error.code` values:

| Code | Meaning |
|---|---|
| `agent_run_failed` | The CLI failed during the turn or exited without a result |
| `agent_start_failed` | The CLI did not start |
| `cancelled` | A client cancelled the run |
| `event_limit` | The run exceeded NATIVE_AGENT_MAX_RUN_EVENTS |
| `native_session_bind_failed` | The native session ID could not be stored |
| `native_session_missing` | The CLI did not report a native session ID |
| `timeout` | The turn exceeded NATIVE_AGENT_QUERY_TIMEOUT_SECONDS |

A turn `result` is:

- `text`: the answer;
- `outcome`: `completed`, `failed`, `interrupted` or `limit_reached`, with `error` (redacted)
  saying why when it is not `completed`;
- `tokens`: `input` (uncached), `output`, `cache_read`, `cache_creation`, summed over the turn's
  model calls. A count the CLI does not report stays absent rather than zero;
- `context {window, used}`: how full the model's context window is after the turn;
- `provider`: the native outcome code (`reason`), the agent's own usage (Claude: turn and
  conversation cost and per-model usage; Codex: thread totals) and the native event.

A **session** is `session_id`, `workspace_id`, `native_session_id`, `state`, `lineage {origin,
fork_from}` (`new` or `fork`; never changes), `active_run {run_id, mode}` while `active`,
`last_run {run_id, mode, status}`, `runs {count, without_turns}`, `usage`, `settings` and `times`.
A run's `mode` is how it opened the native conversation: `new`, `resume` or `fork`.

A **workspace** is `workspace_id`, `storage`, `attachment` and `times {created_at, last_used_at}`;
`last_used_at` is the latest session activity in it.

`GET /v1/agent` groups `agent {name, title, vendor}` (`name` is `claude` or `codex`), `cli {binary, version, tested_version}`,
`profile {dir}`, `login {methods}`, `capabilities` and `gateway {build, allowed_roots}`.

## Workspaces and files

- `storage` is `managed` (a directory the gateway owns) or `attachment` (a host directory).
- `POST /v1/workspaces/create` makes a private managed workspace. `POST /v1/workspaces/attach`
  attaches a directory under `NATIVE_AGENT_ALLOWED_ROOTS`, reported `ready` or `unavailable`.
- `…/{workspace_id}/delete` removes a managed workspace and its files; `…/detach` removes an
  attachment and leaves the directory untouched. Each refuses the other kind with 409.
- Files are under `/v1/workspaces/{workspace_id}/files`: `GET` lists, `GET download/{path}` and
  `POST upload/{path}` transfer, and `POST delete`, `mkdir`, `copy`, `move` change the tree.
  `mkdir`, `copy` and `move` require `conflict`: `error` (409 when the path exists) or `replace`.
  Parent directories are never created implicitly.

## Sessions

The gateway stores only session metadata: workspace, state, fork relation, the last run and its
settings. Native transcripts stay owned by the CLI (reported as `native_cli / unverified`); the
gateway never reads or deletes them.

- `POST /v1/sessions/{session_id}/fork` returns a new session with the parent's settings; its first
  continue forks the native conversation.
- A session is `idle` between runs and `active` while one runs.
- `GET /v1/sessions` filters by `workspace_id`. A cleanly finished run marks its session
  `completed`; an `active` record found after a restart becomes `orphaned`; a session whose
  workspace is inaccessible is `unavailable`.
- `POST /v1/sessions/{session_id}/delete` removes gateway metadata only, and returns 409 while the
  session is active.

## Events

SSE and WebSocket carry the same events:

| Event | Meaning |
|---|---|
| `agent` | Unmodified native CLI event |
| `turn` | A completed turn and its result (`{turn, result}`) |
| `permission_request`, `permission_resolved` | Tool permission lifecycle |
| `control_request`, `control_resolved` | Native control request lifecycle |
| `partial` | Live partial output, only with `?partial=true`; not kept, no sequence ID |
| `diagnostic` | Bounded, redacted adapter debug output requested for the run |
| `done` | Final snapshot of a successful run |
| `error` | Final snapshot of a failed or cancelled run, with `error` |

A `turn` answers a client message; `progress.turns` and `result` count only those. A turn the agent
starts on its own (Claude Code reacting to a background-task notification, for example one left over
in a resumed session) arrives only as `agent` events; its tokens still count in `usage`.

SSE resumes with `Last-Event-ID` or `?after`; a reader that accepts no bytes for a minute is
disconnected. A WebSocket also accepts client messages, selected by `type`. Every one also has a
REST endpoint.

| `type` | Does |
|---|---|
| `message` | Sends the next user message (`prompt`) |
| `interrupt` | Interrupts the current turn |
| `permission` | Answers a permission request (`permission_id`, `behavior`, `scope`) |
| `control` | Sends a native control request; answered with `control_result` and the client's `id` |
| `control_response` | Answers a control request of the agent (`control_id`) |
| `close` | Closes the run's input; the run ends after the current turn |
| `cancel` | Cancels the run |

A rejected message is answered with `control_error` (`control`, `code`, `message`); the codes are
those of the REST endpoints.

## Permissions

`tools.permissions: bypass` lets the CLI use tools without asking. With `ask`, each tool use emits
a `permission_request` and waits. A request has a `permission_id`, a `kind` shared by every agent
(`command`, `file_change`, `mcp`, `other`), a `summary` (the command, file or tool) and a
`provider` section with the native tool or method and request.

Answer with `…/permissions/{permission_id}/allow` (`scope`: `once`, `session` or `workspace`;
optional `provider` section, Claude: `updated_input`) or `…/deny`. Session and workspace rules
allow a `kind`, so they work the same for every agent; they live in server memory and are lost on
restart. Control requests from the agent are answered with `…/native/controls/{control_id}/answer`.

## Agent endpoints

- **Login.** `POST /v1/agent/login/start` runs the CLI's own login with a method from
  `login.methods` (see the table below) and an optional `provider` section (Claude: `email`,
  `sso`). The response gives
  the `flow` and current `step`: open `step.url` in any browser, then with `paste_code` submit the
  page's code to `POST /v1/agent/login/code` (`step.code.submitted` records it), with
  `device_code` enter `step.user_code` on the page, with `browser` finish on the page. Poll
  `GET /v1/agent/login` while `status` is `pending`, until it is `succeeded`, `failed`,
  `cancelled` or `expired`;
  `…/login/cancel` cancels. Only one login runs at a time (no login ID), it ends after
  `NATIVE_AGENT_LOGIN_TIMEOUT_SECONDS`, and the latest stays readable until the next starts.
  `POST /v1/agent/logout` removes the native credentials. Bodies of these endpoints are never
  logged.

  | Agent | `method` | `flow` | Use |
  |---|---|---|---|
  | `claude` | `claudeai` | `paste_code` | Claude subscription |
  | `claude` | `console` | `paste_code` | Anthropic Console (API billing) |
  | `codex` | `chatgpt_device_code` | `device_code` | ChatGPT account; works from any machine and in a container |
  | `codex` | `chatgpt` | `browser` | ChatGPT account; the browser must run on the gateway host |
- **Models.** `GET /v1/agent/models` returns only models the native CLI reports, each with the
  reasoning `efforts` it accepts. An `effort` not listed for its model is rejected; an unknown model
  name is left to the CLI. An adapter that cannot list models returns 501; there is no hard-coded
  catalog.
- **Usage.** `GET /v1/agent/usage` reports the subscription allowance as `windows` (`id`,
  `used_percent`, `resets_at`) plus a `provider` section (Codex: `window_minutes` by window) and
  the native document. It is separate from run token totals.
- **Status.** `GET /v1/agent/status` reports `logged_in` and, when logged in,
  `account {email, plan}`; `provider` has the native login method and status document. Logged out
  is a normal `200`.
- **Diagnostics.** `GET /v1/agent/diagnostics` returns `{format, output}`: `text` for Claude's
  doctor report, `json` for Codex's.

## Monitoring

- Run and session lists filter by lifecycle, workspace and session.
- Finished runs are bounded by a retention time and a count cap; observations and file listings are
  bounded too. Workspaces and session metadata stay until deleted and are listed in pages.
- Observations describe native tasks, subagents and processes: `id`, `kind` (`task`, `subagent`,
  `process`), `state` (`active`, `completed`, `failed`, `cancelled`), `label`, `parent_id` and
  `times`. The run keeps the latest state of each; the native event stays unchanged. An adapter
  reports none when its protocol has no stable IDs and states.
- `GET /health` is a liveness probe; its `status` is always `ok`. `GET /ready` checks the CLI
  command, native login, workspace and session storage, and run capacity. Its `status` is `ready`,
  or `not_ready` with 503 when the gateway cannot accept work. Each component is `healthy`,
  `unhealthy`, or `saturated` (every run slot in use). Both are public and expose no credentials.

## Responses and errors

A successful response is the resource itself, without an envelope. A list is
`{"items": […], "next_cursor": "…"}` with up to 100 items; pass `next_cursor` as `cursor`, with the
same filters, for the next page. The last page has no `next_cursor`.

Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details
(`application/problem+json`):

```json
{
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "validation failed",
  "code": "invalid_request",
  "request_id": "019a5c1e-7b2d-7c41-9f3e-2a8d6b0c4e11",
  "errors": [{"location": "body.model.effort", "message": "expected required property effort to be present"}]
}
```

`code` is stable and meant for client logic. `errors` lists the invalid fields; rejected values are
never echoed.

| Code | Meaning |
|---|---|
| `agent_failed` | The native CLI failed or gave an unusable answer |
| `agent_run_failed` | Ask run: the CLI failed during the turn or exited without a result |
| `agent_start_failed` | Ask run: the CLI did not start |
| `attachment_disabled` | Host-directory attachments need NATIVE_AGENT_ALLOWED_ROOTS |
| `cancelled` | Ask run: a client cancelled the run |
| `control_not_pending` | No pending control request has this ID |
| `control_rejected` | The agent rejected the control request |
| `control_timeout` | The agent did not answer the control request in time |
| `cross_origin_request` | A browser sent a state-changing request from another origin |
| `event_limit` | Ask run: the run exceeded NATIVE_AGENT_MAX_RUN_EVENTS |
| `file_too_large` | The file exceeds NATIVE_AGENT_MAX_FILE_BYTES |
| `forbidden` | The native account may not use this feature |
| `gateway_busy` | All NATIVE_AGENT_MAX_CONCURRENT run slots are in use |
| `host_not_allowed` | Without a token the gateway must be addressed by IP address or localhost |
| `internal_error` | The gateway failed; the log has the request ID |
| `invalid_cursor` | `cursor` does not identify an item in the filtered list |
| `invalid_filter` | A list filter is invalid |
| `invalid_operation` | The file operation is not possible on this path |
| `invalid_path` | The workspace path is absolute, contains `..`, or is otherwise invalid |
| `invalid_request` | The body, a parameter or a field value is invalid; `errors` lists the fields |
| `invalid_request_id` | The X-Request-ID header is not 1-128 allowed characters |
| `invalid_session` | The session ID or the session request is invalid |
| `invalid_workspace` | The workspace ID is invalid |
| `ip_not_allowed` | The client address is not in NATIVE_AGENT_ALLOWED_IPS |
| `login_expired` | The native login has expired; log in again |
| `login_finished` | The login has already finished |
| `login_in_progress` | Another login is starting or pending |
| `login_not_found` | No login has been started |
| `login_required` | The native CLI is not logged in |
| `method_not_allowed` | The path does not support this HTTP method |
| `native_session_bind_failed` | Ask run: the native session ID could not be stored |
| `native_session_missing` | Ask run: the CLI did not report a native session ID |
| `native_session_unavailable` | The session has no native CLI binding |
| `not_acceptable` | No response format matches the Accept header |
| `not_found` | No operation exists at this path, or the file does not exist |
| `path_conflict` | The destination exists and `conflict` is `error` |
| `path_not_allowed` | The path is not a directory inside an allowed root |
| `permission_not_pending` | No pending permission request has this ID |
| `provider_mismatch` | A `provider` section names another agent than the served one |
| `request_timeout` | The client sent the body too slowly |
| `request_too_large` | The body exceeds NATIVE_AGENT_MAX_BODY_BYTES |
| `run_finished` | The run has already finished |
| `run_input_closed` | The run no longer accepts messages |
| `run_not_found` | The run does not exist or its retention has ended |
| `run_not_interactive` | An ask run answers one prompt; continue its session instead |
| `session_busy` | The session has an active run |
| `session_error` | A session operation failed in the gateway |
| `session_exists` | A session with this ID already exists |
| `session_not_found` | The session does not exist |
| `session_not_started` | The session has no native conversation to fork yet |
| `timeout` | Ask run: the turn exceeded NATIVE_AGENT_QUERY_TIMEOUT_SECONDS |
| `too_many_entries` | The directory exceeds NATIVE_AGENT_MAX_LIST_ENTRIES |
| `unauthorized` | A valid bearer token is required |
| `unsupported` | The served agent does not support this operation |
| `unsupported_control` | The WebSocket control message type is unknown |
| `unsupported_media_type` | The Content-Type is not accepted by the operation |
| `workspace_attach_failed` | The gateway could not attach the host directory |
| `workspace_busy` | The workspace has an active session |
| `workspace_create_failed` | The gateway could not create the workspace |
| `workspace_error` | A workspace operation failed in the gateway |
| `workspace_list_failed` | The gateway could not list the workspaces |
| `wrong_workspace_storage` | The operation does not match the workspace storage; use delete or detach |

## Request IDs

Every response has an `X-Request-ID` header. A client can send its own (1–128 characters of
`A-Za-z0-9._:/+=-`); otherwise the gateway creates a time-ordered UUIDv7. An invalid value is
rejected with 400 `invalid_request_id`, not replaced. The ID appears in error bodies, in every log
record of the request, and as `trace.request_id` of a run the request started; the run's log
records carry it too.
