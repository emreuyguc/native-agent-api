# Testing

| Level | Command | Needs |
|---|---|---|
| Unit tests, lint, coverage, vulnerability scan | `make check-pre-commit` | Nothing else; runs in CI |
| Fuzz tests with new inputs | `make test-fuzz FUZZTIME=1m` | Nothing else; runs in CI every week |
| Real CLI tests | `make test-host AGENT=claude\|codex` | The CLI and its login; a few small model calls |
| End-to-end checks | `scripts/acceptance.sh` | A login; starts or uses a gateway |
| Docker image | `docker/verify.sh docker/<name>.env` | Docker with Compose v2 |

`make test` runs each fuzz test only with its seeds and saved inputs. `make test-fuzz` searches
for new inputs. When it finds a failing one, Go saves it under `testdata/fuzz/`; commit it with the fix, so
it stays a regression test.

`make check-coverage` fails when coverage falls below the limit in `.testcoverage.yml`. CI checks
the same limit and writes the coverage to the run summary.

[Development](development.md#checks) lists every check target. The other levels need the real
CLIs, your login or Docker Compose, so CI cannot run them. Run them when a change touches an
adapter, the run lifecycle or the Docker setup. If a check fails, open an issue with the step number, the agent and the command output.

## End-to-end checks

The steps below go through the product once, on the host or in Docker, with one agent. Each says
what you should see. Steps marked **(claude)** or **(codex)** apply to one agent only.

`scripts/acceptance.sh` runs sections 2–9 itself, including the steps that need a second client at
the same time (5.5 watcher, 9.4 limit), and saves every request and a findings file per failure:

```sh
scripts/acceptance.sh -e .env -r <dir under ALLOWED_ROOTS>     # starts the gateway itself
scripts/acceptance.sh -d codex                                   # Docker: setup, login, all sections
scripts/acceptance.sh -u http://127.0.0.1:8701 -m docker         # a container started elsewhere
scripts/acceptance.sh -s "5.5 6"                                  # chosen steps; no -s shows a menu
```

With `-e` (or `-d`) it also restarts the gateway (9.1) and scans its log (8.8); `-a` picks the
agent the started gateway serves. It needs bash 4+, `curl` and `jq`; `websocat` enables 5.7.

## 0. Prerequisites

- macOS or Linux, Go (see `go.mod`), `curl`, `jq`; Docker with Compose v2 for section 10.
- `claude` and/or `codex` installed and signed in on the host (`claude auth status`,
  `codex login status`).
- Optional: `websocat` for the WebSocket steps.

## 1. Host setup

```sh
make build
cp .env.example .env            # mode 0600 is required: chmod 600 .env
./bin/native-agent-api new-token
```

In `.env` set `NATIVE_AGENT_AGENT=claude` (or `codex`), `NATIVE_AGENT_TOKEN_SHA256=<digest>` from
`new-token`, and absolute paths for `NATIVE_AGENT_DATA_DIR` (a fresh directory) and
`NATIVE_AGENT_ALLOWED_ROOTS` (a directory you may attach, e.g. a scratch project). Keep
`NATIVE_AGENT_SANDBOX_MODE=cli`.

```sh
./bin/native-agent-api serve --env-file .env
```

Expected: one JSON log line `gateway started` with `"auth":true`.

In a second terminal:

```sh
export U=http://127.0.0.1:8080 TOK='<client token from new-token>'
api() { curl -sS -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' "$@"; }
```

Pick the model for the rest of the pass (the last one listed is usually the smallest):

```sh
api $U/v1/agent/models | jq '.items[] | {id, efforts}'
export MODEL='{"id":"<id>","effort":"<one of its efforts>"}'
```

## 2. Agent and health

| # | Command | Expected |
|---|---|---|
| 2.1 | `curl -s $U/health` | `200`, no token needed |
| 2.2 | `curl -s $U/ready` | `200` with the CLI version |
| 2.3 | `api $U/v1/agent \| jq '{agent, login, capabilities}'` | The served agent and its capabilities |
| 2.4 | `api $U/v1/agent/status \| jq .logged_in` | `true` |
| 2.5 | `api $U/v1/agent/usage \| jq .` | Native usage/limits, or a clear `501`/`502` problem if the CLI has none |
| 2.6 | `api $U/v1/agent/diagnostics \| jq -r .output \| head` | The native doctor report |
| 2.7 | Open `$U/docs` in a browser | API reference renders; every operation is listed |

## 3. One-shot questions

```sh
# 3.1 Without a workspace: no tools, nothing kept.
api -X POST $U/v1/ask -d "{\"prompt\":\"Reply with exactly PONG.\",\"model\":$MODEL}" | jq '{status, answer: .result.text}'
# Expected: status "succeeded", answer "PONG".

# 3.2 Streamed (SSE): events arrive as they happen, the stream ends with `event: done`.
api -N -X POST "$U/v1/ask/stream" -d "{\"prompt\":\"Count from 1 to 5.\",\"model\":$MODEL}"

# 3.3 Partial output: the same with live text chunks (`event: partial`).
api -N -X POST "$U/v1/ask/stream?partial=true" -d "{\"prompt\":\"Count from 1 to 5.\",\"model\":$MODEL}"

# 3.4 Structured output.
api -X POST $U/v1/ask -d "{\"prompt\":\"Name a color.\",\"model\":$MODEL,\"output\":{\"schema\":{\"type\":\"object\",\"properties\":{\"color\":{\"type\":\"string\"}},\"required\":[\"color\"],\"additionalProperties\":false}}}" | jq -r .result.text
# Expected: a JSON object with a "color" field.
# Codex (OpenAI strict mode) requires "additionalProperties": false on every object.
```

## 4. Workspaces and files

```sh
export WS=$(api -X POST $U/v1/workspaces/create -d '{}' | jq -r .workspace_id)
F=$U/v1/workspaces/$WS/files
api -X POST $F/mkdir -d '{"path":"docs","conflict":"error"}' | jq .path               # 4.1 "docs"
printf 'hello\n' | api -X POST --data-binary @- $F/upload/docs/a.txt | jq .size          # 4.2 6
api $F | jq '.items[].path'                                                               # 4.3 "docs", "docs/a.txt"
api $F/download/docs/a.txt                                                                # 4.4 hello
api -X POST $F/copy -d '{"source":"docs/a.txt","destination":"docs/b.txt","conflict":"error"}' | jq .path   # 4.5
api -X POST $F/move -d '{"source":"docs/b.txt","destination":"c.txt","conflict":"error"}' | jq .path        # 4.6
api -X POST $F/copy -d '{"source":"docs/a.txt","destination":"c.txt","conflict":"error"}' | jq .status      # 4.7 409
api -X POST $F/delete -d '{"path":"c.txt"}' -o /dev/null -w '%{http_code}\n'               # 4.8 204
api $F/download/..%2F..%2Fetc%2Fpasswd | jq .code                                         # 4.9 "invalid_path"
```

4.10 Attach a host directory: `api -X POST $U/v1/workspaces/attach -d '{"path":"<dir under ALLOWED_ROOTS>"}' | jq .`
shows `storage: "attachment"` and `attachment.state: "ready"`. A path outside the allowed roots is
refused with `403`.
Detach it with `POST /v1/workspaces/<id>/detach`; the directory stays untouched.

## 5. Agent runs, sessions and native project features

```sh
# 5.1 Project instructions are loaded (CLAUDE.md for claude, AGENTS.md for codex).
printf 'When asked for the codeword, reply with exactly ZEBRA-42.\n' | api -X POST --data-binary @- $F/upload/CLAUDE.md   # or AGENTS.md
api -X POST $U/v1/ask -d "{\"workspace_id\":\"$WS\",\"prompt\":\"What is the codeword?\",\"model\":$MODEL,\"tools\":{\"permissions\":\"bypass\"}}" | tee /tmp/ask.json | jq -r .result.text
# Expected: ZEBRA-42.

# 5.2 The agent changes files in the workspace.
api -X POST $U/v1/ask -d "{\"workspace_id\":\"$WS\",\"prompt\":\"Create notes.txt containing the word done.\",\"model\":$MODEL,\"tools\":{\"permissions\":\"bypass\"}}" | jq .status
api $F/download/notes.txt                   # Expected: done

# 5.3 Continue the conversation: the agent remembers the earlier turn.
export S=$(jq -r .context.session_id /tmp/ask.json)
api -X POST $U/v1/sessions/$S/continue -d '{"prompt":"What did I ask you first?"}' | jq -r .run_id
# Follow it: api $U/v1/runs/<run_id> | jq '{status, answer: .result.text}'   — mentions the codeword.
# A continued run stays open for more messages: finish it with POST /v1/runs/<run_id>/close.

# 5.4 Fork: a new session with the same history; the parent is unchanged.
export S2=$(api -X POST $U/v1/sessions/$S/fork | jq -r .session_id)
api -X POST $U/v1/sessions/$S2/continue -d '{"prompt":"Repeat the codeword."}' | jq -r .run_id
api $U/v1/sessions | jq '.items[] | {session_id, fork_from: .lineage.fork_from}'
```

5.5 Interactive run with events (two terminals):

```sh
export RUN=$(api -X POST $U/v1/runs/start -d "{\"workspace_id\":\"$WS\",\"prompt\":\"Say hi.\",\"model\":$MODEL,\"tools\":{\"permissions\":\"bypass\"}}" | jq -r .run_id)
api -N "$U/v1/runs/$RUN/events"                        # terminal 2: events, then waits
api -X POST $U/v1/runs/$RUN/message -d '{"prompt":"Now say bye."}'   # a second turn appears in terminal 2
api -X POST $U/v1/runs/$RUN/close                      # the stream ends with `event: done`
```

Reconnect resume: stop the event stream (Ctrl-C) mid-run and reconnect with
`api -N -H 'Last-Event-ID: 3' $U/v1/runs/$RUN/events`; events continue from 4 without duplicates.

5.6 Interrupt and cancel: start a run with a long task ("count slowly to 200"), then
`POST /v1/runs/$RUN/interrupt` (the turn stops, the run stays open) and `POST /v1/runs/$RUN/cancel`
(status `cancelled`; `ps aux | grep -E 'claude|codex'` shows no process of that run).

5.7 WebSocket (optional):

```sh
websocat -H="Authorization: Bearer $TOK" ws://127.0.0.1:8080/v1/runs/$RUN/ws
# then type: {"type":"message","prompt":"Say hi again."}   and later   {"type":"close"}
```

## 6. Permissions

With `tools.permissions: "ask"` the agent asks only where the native CLI asks. Commands that run
inside the CLI sandbox usually do not ask (Claude Code's `autoAllowBashIfSandboxed`, Codex's
`on-request` policy), so the prompt below asks for a tool that does.

```sh
# claude:
PROMPT='Use the Write tool, not Bash, to create perm.txt containing ok.'; EXTRA=''
# codex: network needs escalation; "user" sends it to the API even if your profile sets
# approvals_reviewer = "auto_review" (then Codex's reviewer answers natively and nothing is pending).
PROMPT='Run curl -sI https://example.com with escalated sandbox permissions (it needs network access).'
EXTRA=',"provider":{"name":"codex","approvals_reviewer":"user"}'
export RUN=$(api -X POST $U/v1/runs/start -d "{\"workspace_id\":\"$WS\",\"prompt\":\"$PROMPT\",\"model\":$MODEL,\"tools\":{\"permissions\":\"ask\"}$EXTRA}" | jq -r .run_id)
sleep 10; api $U/v1/runs/$RUN | jq '.waiting.permissions'
# Expected: one request with permission_id, kind, summary and the native tool in provider.
export P=$(api $U/v1/runs/$RUN | jq -r '.waiting.permissions[0].permission_id')
api -X POST $U/v1/runs/$RUN/permissions/$P/allow -d '{"scope":"once"}'
# Expected: 202, the turn continues; claude: perm.txt exists, codex: the curl output shows HTTP 200.
```

Deny: send another message asking for a second file, then
`api -X POST $U/v1/runs/$RUN/permissions/<permission_id>/deny -d '{"message":"no"}'`. Expected: the
file is not created and the agent says the call was declined. Close the run with `POST …/close`.

6.1 Scope `workspace` answers later requests of the same kind automatically;
`GET /v1/workspaces/$WS/permissions` lists the rule, `POST …/permissions/clear` removes it.

## 7. Native controls and options

- 7.1 **(claude)** `api -X POST $U/v1/runs/$RUN/native/control -d '{"request":{"subtype":"get_context_usage"}}'`
  on an open run returns the native context usage.
- 7.2 **(codex)** `api -X POST $U/v1/runs/$RUN/native/control -d '{"request":{"method":"mcpServerStatus/list","params":{}}}'`
  returns the MCP server list.
- 7.3 Provider options: add `"provider":{"name":"claude","max_turns":1}` (claude) or
  `"provider":{"name":"codex","reasoning_summary":"concise"}` (codex) to an ask: accepted.
  The other provider's object is refused with `422 provider_mismatch`.
- 7.4 MCP, skills, hooks, plugins of your native profile work in workspace runs as they do in the
  CLI itself (ask the agent to list its MCP tools).

## 8. Security

| # | Check | Expected |
|---|---|---|
| 8.1 | `curl -s $U/v1/agent` (no token) | `401 unauthorized` |
| 8.2 | `api -X POST -H 'Origin: https://evil.example' $U/v1/workspaces/create -d '{}'` | `403 cross_origin_request` |
| 8.3 | Ask the agent in a workspace: "Use the Write tool to create .mcp.json" **(claude)** / "use apply_patch to create .codex/config.toml" **(codex)**, then run another ask | Claude: the file is not created. Codex: the file may be created but never loads (the next run starts no MCP server from it) |
| 8.4 | `grep -c <data dir> ~/.codex/config.toml` after codex runs **(codex)** | `0`: workspaces are never recorded as trusted |
| 8.5 | Ask the agent: "run `curl -m 5 http://127.0.0.1:8080/health`" | It fails: the CLI sandbox blocks loopback |
| 8.6 | Ask the agent to write a file outside the workspace (e.g. `~/accept-outside.txt`) | Refused by both CLIs. Reading outside the workspace follows the CLI's native sandbox, which allows reads by default (Claude's file tools refuse them) |
| 8.7 | `make test-host AGENT=claude` and `AGENT=codex` | All tests pass |
| 8.8 | Search the gateway log for your token and for `sk-` | Not found |

## 9. Reliability

- 9.1 **Restart:** stop the gateway (Ctrl-C) during a run, start it again. The run is gone, its agent
  process is gone, and `POST /v1/sessions/$S/continue` resumes the conversation.
- 9.2 **Background task (claude):** ask in a workspace "start `sleep 900` as a background task, then
  reply STARTED". The ask returns within `NATIVE_AGENT_QUERY_TIMEOUT_SECONDS` with `succeeded`, and
  the log warns `agent kept running after its input closed`; no `sleep 900` survives.
- 9.3 **Long output (codex):** ask "run `seq 1 20000`". The run succeeds (output deltas are live-only).
- 9.4 **Concurrency:** an open run holds a slot until it ends. Start `NATIVE_AGENT_MAX_CONCURRENT`
  runs (`POST /v1/runs/start`, each in its own workspace) and one more: the extra one is refused with
  `429 gateway_busy`. Close one run and the next start is accepted.

## 10. Docker

`scripts/acceptance.sh -d claude` (or `-d codex`) does this whole section: it prepares the files,
builds and checks the image, starts the container, logs in, runs sections 2–9 against it and then
10.3–10.5. By hand:

Set up the installation as in [docker/README.md](../docker/README.md#setup). Log in inside the container through the API. Host credentials are not mounted:
Claude keeps them in the macOS Keychain, and a shared Codex `auth.json` makes the host and the
container rotate the same refresh token, which signs one of them out.

```sh
docker/verify.sh docker/claude.env               # 10.1 Expected: OK: native-agent-api-claude
C="docker compose --env-file docker/claude.env"
$C up -d --wait
export U=http://127.0.0.1:8701
api -X POST $U/v1/agent/login/start -d '{"method":"claudeai"}' | jq .      # 10.2
# Open the URL, sign in, then: api -X POST $U/v1/agent/login/code -d '{"code":"<code from the page>"}'
```

For codex use `docker/codex.env`, port `8702` and `{"method":"chatgpt_device_code"}` (open `step.url`,
enter `step.user_code`).

Then run sections 2–9 against the container port. Checks that need the host filesystem or process
table do not apply. In a container the agent can reach the gateway on its loopback but gets `401`
without the token (8.5), and Codex has nothing to escalate, so section 6 does not ask with Codex.

- 10.3 `$C restart` keeps the login, workspaces and sessions (named volumes).
- 10.4 Restricted egress: with `COMPOSE_FILE=docker/compose.yaml:docker/compose.egress.yaml` in
  the Docker env file, `docker/verify.sh docker/claude.env` prints `OK: native-agent-api-claude
  (restricted egress)`. After `$C up -d`, an ask still works, while asking the agent to fetch
  `https://example.com` fails and `$C logs egress` shows `refused on filtered domain`.
- 10.5 Cleanup: `$C down` keeps the volumes; `$C down -v` deletes them, including the login.

## 11. Cleanup (host)

Stop the gateway and delete `NATIVE_AGENT_DATA_DIR`. The native profiles are unchanged by the
gateway, except the sessions the CLI itself records.
