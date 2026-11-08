# API concepts

The generated OpenAPI document is the complete contract for routes, fields, responses and
validation: see `/docs`, `/openapi.yaml` or `native-agent-api openapi`. Reads are `GET`; every
change is a `POST` to a path that names the action (`…/delete`, `…/cancel`). `GET /v1/agent`
reports the served CLI, its version, the gateway build and capability identifiers; clients must
ignore capability values they do not recognize.

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
| `error` | `code` and `message` of a failed run |
| `times` | `created_at`, `started_at`, `updated_at`, `finished_at` |
| `trace` | `request_id` of the request that started the run |

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

`GET /v1/agent` groups `agent {name, title, vendor}`, `cli {binary, version, tested_version}`,
`profile {dir}`, `login {methods}`, `capabilities` and `gateway {build, allowed_roots}`.

## Workspaces and files

- `POST /v1/workspaces/create` makes a private managed workspace. `POST /v1/workspaces/attach`
  attaches a directory under `NATIVE_AGENT_ALLOWED_ROOTS`, reported `ready` or `unavailable`.
- `…/{workspace_id}/delete` removes a managed workspace and its files; `…/detach` removes an
  attachment and leaves the directory untouched. Each refuses the other kind with 409.
- Files are under `/v1/workspaces/{workspace_id}/files`: `GET` lists, `GET download/{path}` and
  `POST upload/{path}` transfer, and `POST delete`, `mkdir`, `copy`, `move` change the tree.
  `mkdir`, `copy` and `move` require `conflict: "error"|"replace"`; parent directories are never
  created implicitly.

## Sessions

The gateway stores only session metadata: workspace, state, fork relation, the last run and its
settings. Native transcripts stay owned by the CLI (reported as `native_cli / unverified`); the
gateway never reads or deletes them.

- `POST /v1/sessions/{session_id}/fork` returns a new session with the parent's settings; its first
  continue forks the native conversation.
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
disconnected. A WebSocket also accepts messages (`{"type":"message","prompt":"…"}`), interrupt,
permission answers (`permission_id`), native control requests and answers (`control_id`), close and
cancel. Every WebSocket operation also has a REST endpoint.

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
  `login.methods` and an optional `provider` section (Claude: `email`, `sso`). The response gives
  the `flow` and current `step`: open `step.url` in any browser, then with `paste_code` submit the
  page's code to `POST /v1/agent/login/code` (`step.code.submitted` records it), with
  `device_code` enter `step.user_code` on the page, with `browser` finish on the page. Poll
  `GET /v1/agent/login` until `status` is `succeeded`, `failed`, `cancelled` or `expired`;
  `…/login/cancel` cancels. Only one login runs at a time (no login ID), it ends after
  `NATIVE_AGENT_LOGIN_TIMEOUT_SECONDS`, and the latest stays readable until the next starts.
  `POST /v1/agent/logout` removes the native credentials. Bodies of these endpoints are never
  logged.
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
- `GET /health` is a liveness probe. `GET /ready` checks the CLI command, native login,
  workspace and session storage, and run capacity, and returns 503 with component status when the
  gateway cannot accept work. Both are public and expose no credentials.

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

## Request IDs

Every response has an `X-Request-ID` header. A client can send its own (1–128 characters of
`A-Za-z0-9._:/+=-`); otherwise the gateway creates a time-ordered UUIDv7. An invalid value is
rejected with 400 `invalid_request_id`, not replaced. The ID appears in error bodies, in every log
record of the request, and as `trace.request_id` of a run the request started; the run's log
records carry it too.
