#!/usr/bin/env bash
# Interactive runner for the end-to-end checks in docs/testing.md. It drives a running gateway (or starts one), runs the
# selected steps, does the "watch from another terminal" parts in parallel itself, and writes every
# request/response plus a findings file for each failure.
#
#   scripts/acceptance.sh                       # menu; uses http://127.0.0.1:8080
#   scripts/acceptance.sh -e .env               # start the gateway from .env (enables restart/log checks)
#   scripts/acceptance.sh -d codex              # Docker: set up, build, log in, test, restart, egress
#   scripts/acceptance.sh -u http://127.0.0.1:8701 -m docker -s "2 3 5.5"
#
# Options:
#   -u URL        Gateway base URL (default: from -e LISTEN, else http://127.0.0.1:8080)
#   -e FILE       Env file: start bin/native-agent-api with it and read limits from it
#   -a AGENT      With -e: serve claude or codex, overriding NATIVE_AGENT_AGENT (default: asked)
#   -s "IDS"      Steps or sections to run, e.g. "all", "2 4", "5.5 6.1" (default: menu)
#   -d AGENT      Docker: prepare docker/AGENT.env and its application env file, build, start
#                 and log in the claude or codex container, run the sections, then the Docker
#                 steps (10.x)
#   -m MODE       host (default) or docker (a container started elsewhere); docker skips checks
#                 that need the host filesystem or process table
#   -r DIR        Directory under NATIVE_AGENT_ALLOWED_ROOTS for the attach step (4.10)
#   -M ID         Model id (default: the last listed model with its lowest effort)
#   -y            Never prompt: manual steps are skipped, slow steps run
# The client token comes from NATIVE_AGENT_CLIENT_TOKEN or is asked for (not echoed, never logged).
set -uo pipefail

if ((BASH_VERSINFO[0] < 4)); then
  echo "bash 4 or newer is required (macOS: brew install bash), found $BASH_VERSION" >&2
  exit 2
fi
for tool in curl jq; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
URL="" ENV_FILE="" SELECT="" MODE=host ATTACH_DIR="" MODEL_ID="" YES=false SERVE_AGENT="" DOCKER_AGENT=""
while getopts "u:e:a:d:s:m:r:M:yh" opt; do
  case $opt in
    u) URL=${OPTARG%/} ;; e) ENV_FILE=$OPTARG ;; a) SERVE_AGENT=$OPTARG ;; d) DOCKER_AGENT=$OPTARG MODE=docker ;; s) SELECT=$OPTARG ;; m) MODE=$OPTARG ;;
    r) ATTACH_DIR=$OPTARG ;; M) MODEL_ID=$OPTARG ;; y) YES=true ;;
    *) awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; exit 0 ;;
  esac
done
[[ $MODE == host || $MODE == docker ]] || { echo "-m must be host or docker" >&2; exit 2; }
[[ -z $DOCKER_AGENT || $DOCKER_AGENT == claude || $DOCKER_AGENT == codex ]] || { echo "-d must be claude or codex" >&2; exit 2; }
[[ -z $DOCKER_AGENT || -z $ENV_FILE ]] || { echo "-d and -e exclude each other" >&2; exit 2; }

if [[ -t 1 ]]; then
  G=$'\e[32m' R=$'\e[31m' Y=$'\e[33m' B=$'\e[1m' D=$'\e[2m' N=$'\e[0m'
else
  G="" R="" Y="" B="" D="" N=""
fi

envval() { # envval NAME DEFAULT: a value from the env file
  local value=""
  [[ -n $ENV_FILE ]] && value=$(grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2-)
  echo "${value:-$2}"
}

# ---------------------------------------------------------------- results

OUT="$PWD/acceptance-results/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"
LOG="$OUT/requests.log" FINDINGS="$OUT/findings.md" RES="$OUT/.response" GW_LOG=""
PASSED=0 FAILED=0 SKIPPED=0 FINDING=0 STEP="-"
: >"$LOG"

pass() { PASSED=$((PASSED + 1)); printf '  %s✔%s %s\n' "$G" "$N" "$1"; }
skip() { SKIPPED=$((SKIPPED + 1)); printf '  %s–%s %s %s(%s)%s\n' "$Y" "$N" "$1" "$D" "$2" "$N"; }
fail() { # fail DESCRIPTION EXPECTED ACTUAL
  FAILED=$((FAILED + 1)) FINDING=$((FINDING + 1))
  printf '  %s✘ %s%s\n      expected: %s\n      actual:   %s\n' "$R" "$1" "$N" "$2" "${3:0:300}"
  local request_id
  request_id=$(jq -r '.request_id // empty' "$RES" 2>/dev/null)
  cat >>"$FINDINGS" <<EOF
ID: A-$FINDING   Section: $STEP   Agent: $AGENT   Mode: $MODE
Check: $1
Expected: $2
Actual: ${3:0:2000}
Request: ${request_id:-see requests.log, step $STEP}

EOF
}
expect() { # expect DESCRIPTION EXPECTED ACTUAL
  if [[ $3 == "$2" ]]; then pass "$1"; else fail "$1" "$2" "$3"; fi
}
expect_match() { # expect_match DESCRIPTION REGEX ACTUAL
  if [[ $3 =~ $2 ]]; then pass "$1"; else fail "$1" "matches /$2/" "$3"; fi
}

# wants SECTION: true when any step of the section is selected.
wants() {
  local want
  for want in $SELECT; do [[ $want == all || $want == "$1" || $want == "$1".* ]] && return 0; done
  return 1
}

# step ID TITLE: true when the step is selected; prints its header.
step() {
  local id=$1 selected=false want
  for want in $SELECT; do
    [[ $want == all || $id == "$want" || $id == "$want".* ]] && selected=true
  done
  $selected || return 1
  STEP=$id
  printf '\n%s%s %s%s\n' "$B" "$id" "$2" "$N"
  echo "=== $id $2" >>"$LOG"
}

ask_user() { # ask_user QUESTION: y → 0, n → 1, s/-y → 2
  $YES && return 2
  local answer
  read -r -p "  ? $1 [y/n/s=skip] " answer </dev/tty
  case $answer in y | Y) return 0 ;; n | N) return 1 ;; *) return 2 ;; esac
}

# ---------------------------------------------------------------- HTTP

CODE=""
# api METHOD PATH [JSON]: response body in $RES, status in $CODE.
api() {
  local args=(-sS -o "$RES" -w '%{http_code}' -X "$1" --max-time "${TIMEOUT:-900}" -H "Authorization: Bearer $TOKEN")
  [[ $# -ge 3 ]] && args+=(-H 'Content-Type: application/json' --data-binary "$3")
  CODE=$(curl "${args[@]}" "$URL$2" 2>>"$LOG") || CODE=${CODE:-000}
  { echo "--- $1 $2 -> $CODE"; [[ $# -ge 3 ]] && echo "> $3"; head -c 3000 "$RES"; echo; } >>"$LOG"
}
upload() { # upload WORKSPACE PATH CONTENT
  CODE=$(printf '%s' "$3" | curl -sS -o "$RES" -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" \
    --data-binary @- "$URL/v1/workspaces/$1/files/upload/$2" 2>>"$LOG")
  echo "--- POST upload $2 -> $CODE" >>"$LOG"
}
j() { jq -r "$1" "$RES" 2>/dev/null; }

# body PROMPT [WORKSPACE PERMISSIONS [EXTRA_JSON]]: an ask/start request body.
body() {
  jq -nc --arg p "$1" --argjson m "$MODEL" --arg ws "${2-}" --arg perm "${3:-bypass}" --argjson extra "${4:-{\}}" \
    '{prompt: $p, model: $m} + (if $ws != "" then {workspace_id: $ws, tools: {permissions: $perm}} else {} end) + $extra'
}
ask() { api POST /v1/ask "$(body "$@")"; }
start_run() { api POST /v1/runs/start "$(body "$@")"; j .run_id; }
answer() { j '.result.text // empty'; }

# wait_run RUN JQ_CONDITION SECONDS: poll until the condition holds; the snapshot stays in $RES.
wait_run() {
  local deadline=$((SECONDS + $3))
  while ((SECONDS < deadline)); do
    api GET "/v1/runs/$1"
    jq -e "$2" "$RES" >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}
# stream_bg RUN FILE [LAST_EVENT_ID]: follow the run's SSE stream in the background; prints the pid.
stream_bg() {
  local args=(-sN -H "Authorization: Bearer $TOKEN" --max-time 900)
  [[ -n ${3-} ]] && args+=(-H "Last-Event-ID: $3")
  curl "${args[@]}" "$URL/v1/runs/$1/events" >"$2" 2>/dev/null &
  echo $!
}
gateway_pid() {
  local port=${URL##*:}
  lsof -nP -t -iTCP:"${port%%/*}" -sTCP:LISTEN 2>/dev/null | head -1
}
descendants() { # all descendant pids of $1
  local child
  for child in $(pgrep -P "$1" 2>/dev/null); do echo "$child"; descendants "$child"; done
}

# ---------------------------------------------------------------- gateway

GW_PID=""
start_gateway() {
  GW_LOG="$OUT/gateway.log"
  NATIVE_AGENT_AGENT=$SERVE_AGENT "$ROOT/bin/native-agent-api" serve --env-file "$ENV_FILE" >>"$GW_LOG" 2>&1 &
  GW_PID=$!
  local i
  for i in $(seq 1 30); do
    curl -s -o /dev/null "$URL/health" && return 0
    kill -0 "$GW_PID" 2>/dev/null || break
    sleep 0.5
  done
  echo "the gateway did not start; see $GW_LOG" >&2
  tail -5 "$GW_LOG" >&2
  exit 1
}
stop_gateway() {
  [[ -n $GW_PID ]] || return 0
  kill "$GW_PID" 2>/dev/null
  wait "$GW_PID" 2>/dev/null
  GW_PID=""
}
cleanup() {
  local job
  for job in $(jobs -p); do kill "$job" 2>/dev/null; done
  stop_gateway
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------- docker

COMPOSE_ENV="" EGRESS_ENV=""
compose() { docker compose --env-file "$COMPOSE_ENV" "$@"; }
kv() { grep -E "^$2=" "$1" 2>/dev/null | tail -1 | cut -d= -f2-; }
set_kv() { # set_kv FILE KEY VALUE: replace or append KEY=VALUE, keeping the file mode
  local tmp
  tmp=$(mktemp)
  awk -v k="$2" -v v="$3" 'BEGIN { done = 0 } $0 ~ "^" k "=" { print k "=" v; done = 1; next } { print }
    END { if (!done) print k "=" v }' "$1" >"$tmp" && cat "$tmp" >"$1"
  rm -f "$tmp"
}

# docker/compose.yaml sets these for a container, so its application env file leaves them out.
DOCKER_FIXED='AGENT|SANDBOX_MODE|LISTEN|DATA_DIR|ALLOWED_ROOTS|PROFILE_DIR'

docker_prepare() {
  command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
  docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 2; }
  [[ -x $ROOT/bin/native-agent-api ]] || { echo "run make build first (new-token)" >&2; exit 2; }
  local app token_file port
  COMPOSE_ENV="$ROOT/docker/$DOCKER_AGENT.env"
  port=8701
  [[ $DOCKER_AGENT == codex ]] && port=8702
  if [[ ! -f $COMPOSE_ENV ]]; then
    cp "$ROOT/docker/.env.example" "$COMPOSE_ENV"
    chmod 600 "$COMPOSE_ENV"
    set_kv "$COMPOSE_ENV" COMPOSE_PROJECT_NAME "native-agent-api-$DOCKER_AGENT"
    set_kv "$COMPOSE_ENV" CONTAINER_AGENT "$DOCKER_AGENT"
    set_kv "$COMPOSE_ENV" CONTAINER_PUBLISH_PORT "$port"
    echo "created docker/$DOCKER_AGENT.env from the example"
  fi
  [[ $(kv "$COMPOSE_ENV" CONTAINER_AGENT) == "$DOCKER_AGENT" ]] ||
    { echo "$COMPOSE_ENV: CONTAINER_AGENT must be $DOCKER_AGENT" >&2; exit 2; }

  app=$(kv "$COMPOSE_ENV" CONTAINER_APP_ENV_FILE)
  [[ $app == /* ]] || app="$ROOT/docker/$app"
  token_file="$ROOT/docker/$DOCKER_AGENT.token.local.env"
  if [[ ! -f $app ]]; then
    app="$ROOT/docker/$DOCKER_AGENT.app.local.env"
    if [[ ! -f $app ]]; then
      local generated digest
      generated=$("$ROOT/bin/native-agent-api" new-token)
      digest=$(grep -o 'SHA256=.*' <<<"$generated" | cut -d= -f2)
      (umask 077; grep '^NATIVE_AGENT_' "$ROOT/.env.example" | grep -vE "^NATIVE_AGENT_($DOCKER_FIXED)=" >"$app")
      set_kv "$app" NATIVE_AGENT_TOKEN_SHA256 "$digest"
      (umask 077; sed -n 2p <<<"$generated" | tr -d ' ' >"$token_file")
      echo "created $app and its client token in $token_file"
    fi
    set_kv "$COMPOSE_ENV" CONTAINER_APP_ENV_FILE "$app"
  fi
  # The same installation with the restricted egress overlay, for 10.4.
  EGRESS_ENV="$OUT/docker-egress.env"
  (umask 077; cp "$COMPOSE_ENV" "$EGRESS_ENV")
  set_kv "$EGRESS_ENV" COMPOSE_FILE docker/compose.yaml:docker/compose.egress.yaml
  [[ -z ${NATIVE_AGENT_CLIENT_TOKEN-} && -f $token_file ]] && NATIVE_AGENT_CLIENT_TOKEN=$(cat "$token_file")
  URL="http://$(kv "$COMPOSE_ENV" CONTAINER_PUBLISH_ADDR):$(kv "$COMPOSE_ENV" CONTAINER_PUBLISH_PORT)"
  URL=${URL/0.0.0.0/127.0.0.1}
}

wait_health() {
  local i
  for i in $(seq 1 60); do
    curl -s -o /dev/null "$URL/health" && return 0
    sleep 1
  done
  return 1
}

docker_start() {
  printf '\n%s10.1 Build, hardening and start%s\n' "$B" "$N"
  STEP=10.1
  if "$ROOT/docker/verify.sh" "$COMPOSE_ENV" >"$OUT/10.1-verify.log" 2>&1; then
    pass "image builds and passes docker/verify.sh"
  else
    fail "image builds and passes docker/verify.sh" "OK" "$(tail -3 "$OUT/10.1-verify.log")"
    exit 1
  fi
  { compose up -d --wait >>"$OUT/10.1-verify.log" 2>&1 && wait_health; } || { echo "the container did not start" >&2; exit 1; }
  pass "container healthy on $URL"
  compose exec -T agent mkdir -p /projects/acceptance
  [[ -n $ATTACH_DIR ]] || ATTACH_DIR=/projects/acceptance
}

docker_login() {
  printf '\n%s10.2 Native login inside the container%s\n' "$B" "$N"
  STEP=10.2
  api GET /v1/agent/status
  if [[ $(j .logged_in) == true ]]; then
    pass "already logged in (the login lives in the $(kv "$COMPOSE_ENV" COMPOSE_PROJECT_NAME)-home volume)"
    return 0
  fi
  if $YES; then
    fail "logged in" "a login (run without -y to log in)" "logged_in=false"
    exit 1
  fi
  local method code status i
  method=claudeai
  [[ $DOCKER_AGENT == codex ]] && method=chatgpt_device_code
  api POST /v1/agent/login/start "$(jq -nc --arg m "$method" '{method: $m}')"
  [[ $CODE == 2* ]] || { fail "login starts" "2xx" "$CODE $(cat "$RES")"; exit 1; }
  echo "  Open this URL in a browser and sign in:"
  echo "    $(j .step.url)"
  [[ -n $(j '.step.user_code // empty') ]] && echo "  Enter the code: $(j .step.user_code)"
  if [[ $(j .flow) == paste_code ]]; then
    read -r -p "  Paste the code shown after sign-in: " code </dev/tty
    api POST /v1/agent/login/code "$(jq -nc --arg c "$code" '{code: $c}')"
  fi
  for i in $(seq 1 180); do
    api GET /v1/agent/login
    status=$(j .status)
    [[ $status == pending ]] || break
    sleep 5
  done
  expect "login finished" succeeded "$status"
  api GET /v1/agent/status
  [[ $(j .logged_in) == true ]] || { fail "logged in" true "$(j .logged_in)"; exit 1; }
  pass "logged in"
}

# restart_gateway: restart the served gateway (host: the process this script started).
restart_gateway() {
  if [[ $MODE == docker && -n $DOCKER_AGENT ]]; then
    compose restart agent >/dev/null 2>&1 && wait_health
  else
    stop_gateway
    start_gateway
  fi
}
can_restart() { [[ -n $GW_PID || -n $DOCKER_AGENT ]]; }

docker_steps() {
  if step 10.3 "Restart keeps the login, workspaces and sessions"; then
    ensure_session
    local run
    compose restart agent >/dev/null 2>&1 && wait_health
    api GET /v1/agent/status
    expect "still logged in" true "$(j .logged_in)"
    api GET "/v1/workspaces/$WS"
    expect "the workspace survives" 200 "$CODE"
    api POST "/v1/sessions/$SESSION/continue" '{"prompt":"Do not run any command. Just tell me the codeword."}'
    run=$(j .run_id)
    wait_run "$run" '.progress.turns >= 1' 300
    expect_match "the session continues" ZEBRA-42 "$(answer)"
    close_run "$run"
  fi
  if step 10.4 "Restricted egress overlay"; then
    ask_user "Restart the container with only the provider's hosts reachable (docker/compose.egress.yaml)?"
    if [[ $? == 1 ]]; then
      skip "restricted egress" "declined"
    else
      compose down >>"$OUT/10.4-verify.log" 2>&1
      if "$ROOT/docker/verify.sh" "$EGRESS_ENV" >"$OUT/10.4-verify.log" 2>&1; then
        pass "docker/verify.sh with the egress overlay: direct egress closed, unlisted host filtered, provider reachable"
      else
        fail "docker/verify.sh with the egress overlay" "OK" "$(tail -3 "$OUT/10.4-verify.log")"
      fi
      local open_env=$COMPOSE_ENV
      COMPOSE_ENV=$EGRESS_ENV
      compose up -d --wait >>"$OUT/10.4-verify.log" 2>&1 && wait_health
      ensure_workspace
      ask "Reply with exactly PONG."
      expect_match "the provider API works through the proxy" PONG "$(answer)"
      ask "Run: curl -s -m 10 -o /dev/null -w '%{http_code}' https://example.com . Reply with exactly FETCHED if it printed 200, otherwise BLOCKED." "$WS"
      expect_match "an unlisted host is blocked" BLOCKED "$(answer)"
      compose down >>"$OUT/10.4-verify.log" 2>&1
      COMPOSE_ENV=$open_env
      compose up -d --wait >>"$OUT/10.4-verify.log" 2>&1 && wait_health
      pass "back to unrestricted egress"
    fi
  fi
  if step 10.5 "Stop"; then
    ask_user "Stop the container now? Volumes (login, workspaces) are kept."
    case $? in
      0) compose down >/dev/null 2>&1; pass "stopped; volumes kept" ;;
      *) skip "stop" "left running on $URL" ;;
    esac
    echo "  To delete everything, including the login: docker compose --env-file docker/$DOCKER_AGENT.env down -v"
  fi
}

if [[ -n $DOCKER_AGENT ]]; then
  docker_prepare
fi

if [[ -z $URL ]]; then
  listen=$(envval NATIVE_AGENT_LISTEN 127.0.0.1:8080)
  listen=${listen/0.0.0.0/127.0.0.1}
  URL="http://$listen"
fi
if [[ -n $ENV_FILE ]]; then
  [[ -x $ROOT/bin/native-agent-api ]] || { echo "run make build first" >&2; exit 2; }
  if curl -s -o /dev/null "$URL/health"; then
    echo "a gateway already answers on $URL; stop it or drop -e" >&2
    exit 2
  fi
  if [[ -z $SERVE_AGENT ]]; then
    SERVE_AGENT=$(envval NATIVE_AGENT_AGENT claude)
    if ! $YES; then
      read -r -p "Agent to serve [claude/codex] ($SERVE_AGENT): " choice </dev/tty
      SERVE_AGENT=${choice:-$SERVE_AGENT}
    fi
  fi
  [[ $SERVE_AGENT == claude || $SERVE_AGENT == codex ]] || { echo "-a must be claude or codex" >&2; exit 2; }
  start_gateway
fi
if [[ -n $DOCKER_AGENT ]]; then
  docker_start
fi
curl -s -o /dev/null "$URL/health" || { echo "no gateway answers on $URL" >&2; exit 2; }

TOKEN=${NATIVE_AGENT_CLIENT_TOKEN-}
if [[ -z $TOKEN ]]; then
  read -r -s -p "Client token (empty if the gateway runs without one): " TOKEN </dev/tty
  echo
fi
api GET /v1/agent
[[ $CODE == 200 ]] || { echo "GET /v1/agent returned $CODE: $(cat "$RES")" >&2; exit 2; }
AGENT=$(j .agent.name)
[[ -n $DOCKER_AGENT ]] && docker_login

api GET /v1/agent/models
if [[ -n $MODEL_ID ]]; then
  MODEL=$(jq -c --arg id "$MODEL_ID" '(.items[] | select(.id == $id) | {id, effort: (.efforts[0] // "")}) // {id: $id}' "$RES")
else
  MODEL=$(jq -c '.items[-1] | {id, effort: (.efforts[0] // "")}' "$RES")
fi
MODEL=$(jq -c 'if .effort == "" then del(.effort) else . end' <<<"$MODEL")
MAX_CONCURRENT=$(envval NATIVE_AGENT_MAX_CONCURRENT 2)
QUERY_TIMEOUT=$(envval NATIVE_AGENT_QUERY_TIMEOUT_SECONDS 480)
INSTRUCTIONS=CLAUDE.md
[[ $AGENT == codex ]] && INSTRUCTIONS=AGENTS.md

# ---------------------------------------------------------------- shared state

WS="" SESSION=""
ensure_workspace() {
  [[ -n $WS ]] && return 0
  api POST /v1/workspaces/create '{}'
  WS=$(j .workspace_id)
}
new_workspace() { api POST /v1/workspaces/create '{}'; j .workspace_id; }
ensure_session() { # a session whose first turn asked for the codeword
  [[ -n $SESSION ]] && return 0
  ensure_workspace
  upload "$WS" "$INSTRUCTIONS" $'When asked for the codeword, reply with exactly ZEBRA-42.\n'
  ask "What is the codeword?" "$WS"
  SESSION=$(j '.context.session_id // empty')
}
close_run() { api POST "/v1/runs/$1/close"; wait_run "$1" '.status != "running"' 60 >/dev/null; }

# ---------------------------------------------------------------- sections

section_2() {
  if step 2.1 "Health without a token"; then
    expect "/health is 200" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$URL/health")"
  fi
  if step 2.2 "Readiness"; then
    expect "/ready reports ready" ready "$(curl -s "$URL/ready" | jq -r .status)"
  fi
  if step 2.3 "Agent description"; then
    api GET /v1/agent
    expect "agent name" "$AGENT" "$(j .agent.name)"
    expect_match "login methods listed" '.+' "$(j '.login.methods | join(",")')"
  fi
  if step 2.4 "Login status"; then
    api GET /v1/agent/status
    expect "logged in" true "$(j .logged_in)"
  fi
  if step 2.5 "Usage"; then
    api GET /v1/agent/usage
    expect_match "native usage or a clear problem" '^(200|501|502)$' "$CODE"
  fi
  if step 2.6 "Diagnostics"; then
    TIMEOUT=120 api GET /v1/agent/diagnostics
    expect "doctor report returned" 200 "$CODE"
  fi
  if step 2.7 "API reference in a browser"; then
    ask_user "Open $URL/docs in a browser. Does it render with every operation?"
    case $? in 0) pass "reference renders" ;; 1) fail "reference renders" "rendered page" "owner: no" ;; *) skip "reference renders" "manual" ;; esac
  fi
}

section_3() {
  if step 3.1 "Ask without a workspace"; then
    ask "Reply with exactly PONG."
    expect "status" succeeded "$(j .status)"
    expect_match "answer" PONG "$(answer)"
  fi
  if step 3.2 "Streamed ask (SSE)"; then
    curl -sN -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --max-time 300 \
      -X POST "$URL/v1/ask/stream" --data-binary "$(body "Count from 1 to 5.")" >"$OUT/3.2.sse"
    expect_match "agent events arrive" 'event: agent' "$(cat "$OUT/3.2.sse")"
    expect "ends with done" "event: done" "$(grep '^event:' "$OUT/3.2.sse" | tail -1)"
  fi
  if step 3.3 "Partial output"; then
    curl -sN -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --max-time 300 \
      -X POST "$URL/v1/ask/stream?partial=true" --data-binary "$(body "Count from 1 to 5.")" >"$OUT/3.3.sse"
    expect_match "partial chunks arrive" 'event: partial' "$(cat "$OUT/3.3.sse")"
  fi
  if step 3.4 "Structured output"; then
    api POST /v1/ask "$(body "Name a color." "" "" '{"output":{"schema":{"type":"object","properties":{"color":{"type":"string"}},"required":["color"],"additionalProperties":false}}}')"
    expect "answer is an object with color" string "$(answer | jq -r '.color | type' 2>/dev/null)"
  fi
}

section_4() {
  wants 4 || return 0
  ensure_workspace
  local file_api="/v1/workspaces/$WS/files"
  if step 4.1 "mkdir"; then
    api POST "$file_api/mkdir" '{"path":"docs","conflict":"error"}'
    expect "created docs" docs "$(j .path)"
  fi
  if step 4.2 "upload"; then
    upload "$WS" docs/a.txt $'hello\n'
    expect "6 bytes stored" 6 "$(j .size)"
  fi
  if step 4.3 "list"; then
    api GET "$file_api"
    expect_match "docs/a.txt listed" 'docs/a\.txt' "$(j '[.items[].path] | join(" ")')"
  fi
  if step 4.4 "download"; then
    api GET "$file_api/download/docs/a.txt"
    expect "content" hello "$(cat "$RES")"
  fi
  if step 4.5 "copy, move, conflict, delete"; then
    api POST "$file_api/copy" '{"source":"docs/a.txt","destination":"docs/b.txt","conflict":"error"}'
    expect "copy" docs/b.txt "$(j .path)"
    api POST "$file_api/move" '{"source":"docs/b.txt","destination":"c.txt","conflict":"error"}'
    expect "move" c.txt "$(j .path)"
    api POST "$file_api/copy" '{"source":"docs/a.txt","destination":"c.txt","conflict":"error"}'
    expect "copy onto an existing file is refused" 409 "$CODE"
    api POST "$file_api/delete" '{"path":"c.txt"}'
    expect "delete" 204 "$CODE"
  fi
  if step 4.9 "Path traversal"; then
    api GET "$file_api/download/..%2F..%2Fetc%2Fpasswd"
    expect "refused" invalid_path "$(j .code)"
  fi
  if step 4.10 "Attach a host directory"; then
    if [[ -z $ATTACH_DIR ]]; then
      skip "attach" "pass -r <dir under NATIVE_AGENT_ALLOWED_ROOTS>"
    else
      api POST /v1/workspaces/attach "$(jq -nc --arg p "$ATTACH_DIR" '{path: $p}')"
      expect "attached and ready" "attachment ready" "$(j '"\(.storage) \(.attachment.state)"')"
      local attached
      attached=$(j .workspace_id)
      api POST /v1/workspaces/attach '{"path":"/etc"}'
      expect "a path outside the allowed roots is refused" 403 "$CODE"
      api POST "/v1/workspaces/$attached/detach"
      expect_match "detach" '^2' "$CODE"
    fi
  fi
}

section_5() {
  if step 5.1 "Project instructions ($INSTRUCTIONS)"; then
    SESSION=""
    ensure_session
    expect_match "codeword from $INSTRUCTIONS" ZEBRA-42 "$(answer)"
  fi
  if step 5.2 "The agent writes a file"; then
    ensure_workspace
    ask "Create notes.txt containing the word done." "$WS"
    expect "status" succeeded "$(j .status)"
    api GET "/v1/workspaces/$WS/files/download/notes.txt"
    expect_match "notes.txt" "done" "$(cat "$RES")"
  fi
  if step 5.3 "Continue the session"; then
    ensure_session
    api POST "/v1/sessions/$SESSION/continue" '{"prompt":"What did I ask you first? Quote it."}'
    local run
    run=$(j .run_id)
    if wait_run "$run" '.progress.turns >= 1' 300; then
      expect_match "remembers the first question" '[Cc]odeword' "$(answer)"
    else
      fail "continued turn finishes" "turns >= 1 within 300s" "$(j -c '{status, turns}')"
    fi
    close_run "$run"
  fi
  if step 5.4 "Fork the session"; then
    ensure_session
    api POST "/v1/sessions/$SESSION/fork"
    local fork run
    fork=$(j .session_id)
    expect "fork_from points at the parent" "$SESSION" "$(j .lineage.fork_from)"
    api POST "/v1/sessions/$fork/continue" '{"prompt":"Repeat the codeword."}'
    run=$(j .run_id)
    wait_run "$run" '.progress.turns >= 1' 300
    expect_match "the fork has the parent's history" ZEBRA-42 "$(answer)"
    close_run "$run"
  fi
  if step 5.5 "Interactive run while a second client watches (parallel)"; then
    ensure_workspace
    local run watcher
    run=$(start_run "Say hi." "$WS")
    watcher=$(stream_bg "$run" "$OUT/5.5.sse")
    wait_run "$run" '.progress.turns >= 1' 300
    api POST "/v1/runs/$run/message" '{"prompt":"Now say bye."}'
    expect "second message accepted" 202 "$CODE"
    wait_run "$run" '.progress.turns >= 2' 300
    api POST "/v1/runs/$run/close"
    expect "close accepted" 202 "$CODE"
    local i
    for i in $(seq 1 30); do kill -0 "$watcher" 2>/dev/null || break; sleep 1; done
    kill "$watcher" 2>/dev/null
    expect "the watcher saw both turns" 2 "$(grep -c '^event: turn' "$OUT/5.5.sse")"
    expect "the watcher's stream ended with done" "event: done" "$(grep '^event:' "$OUT/5.5.sse" | tail -1)"
    curl -sN -H "Authorization: Bearer $TOKEN" -H 'Last-Event-ID: 3' --max-time 30 "$URL/v1/runs/$run/events" >"$OUT/5.5-resume.sse"
    expect "reconnect resumes after the last seen event" "id: 4" "$(grep -m1 '^id:' "$OUT/5.5-resume.sse")"
  fi
  if step 5.6 "Interrupt and cancel"; then
    ensure_workspace
    local run before gw
    gw=$(gateway_pid)
    run=$(start_run "Run this shell command and wait for it: for i in \$(seq 1 200); do echo \$i; sleep 1; done" "$WS")
    sleep 15
    api POST "/v1/runs/$run/interrupt"
    expect "interrupt accepted" 202 "$CODE"
    if wait_run "$run" '.progress.turns >= 1' 60; then
      expect "the run stays open after an interrupt" running "$(j .status)"
    else
      fail "the interrupted turn ends" "turns >= 1 within 60s" "$(j -c '{status, turns}')"
    fi
    api POST "/v1/runs/$run/message" '{"prompt":"Run: sleep 300"}'
    sleep 10
    before=""
    [[ $MODE == host && -n $gw ]] && before=$(descendants "$gw" | tr '\n' ' ')
    api POST "/v1/runs/$run/cancel"
    wait_run "$run" '.status != "running"' 60
    expect "cancelled" cancelled "$(j .status)"
    if [[ -n $before ]]; then
      sleep 2
      local alive="" pid
      for pid in $before; do kill -0 "$pid" 2>/dev/null && alive+="$pid "; done
      expect "no process of the cancelled run survives" "" "$alive"
    else
      skip "process check" "host mode with a local gateway only"
    fi
  fi
  if step 5.7 "WebSocket"; then
    if ! command -v websocat >/dev/null; then
      skip "WebSocket run" "install websocat (brew install websocat)"
    else
      ensure_workspace
      local run
      run=$(start_run "Say hi." "$WS")
      wait_run "$run" '.progress.turns >= 1' 300
      { echo '{"type":"message","prompt":"Say hi again."}'; sleep 25; echo '{"type":"close"}'; sleep 10; } |
        websocat -H="Authorization: Bearer $TOKEN" "${URL/http/ws}/v1/runs/$run/ws" >"$OUT/5.7.ws" 2>&1
      expect_match "turn events over WebSocket" '"type":"turn"' "$(cat "$OUT/5.7.ws")"
      api GET "/v1/runs/$run"
      expect "message and close over WebSocket" "2 true" "$(j '"\(.progress.turns) \(.progress.input_closed)"')"
    fi
  fi
}

# A prompt that makes the native CLI ask for approval, and the options needed for it.
permission_prompt() {
  if [[ $AGENT == claude ]]; then
    PERM_PROMPT="Use the Write tool, not Bash, to create $1 containing ok."
    PERM_EXTRA='{}'
  else
    PERM_PROMPT="Run curl -sI https://example.com with escalated sandbox permissions (it needs network access)."
    PERM_EXTRA='{"provider":{"name":"codex","approvals_reviewer":"user"}}'
  fi
}
section_6() {
  wants 6 || return 0
  ensure_workspace
  if step 6.1 "Allow once"; then
    permission_prompt perm.txt
    local run request
    run=$(start_run "$PERM_PROMPT" "$WS" ask "$PERM_EXTRA")
    if wait_run "$run" '(.waiting.permissions | length) > 0' 120; then
      request=$(j '.waiting.permissions[0].permission_id')
      pass "the agent asks ($(j '.waiting.permissions[0].kind'))"
      api POST "/v1/runs/$run/permissions/$request/allow" '{"scope":"once"}'
      expect "allow accepted" 202 "$CODE"
      wait_run "$run" '.progress.turns >= 1' 300
      if [[ $AGENT == claude ]]; then
        api GET "/v1/workspaces/$WS/files/download/perm.txt"
        expect "the allowed write happened" ok "$(tr -d '\n' <"$RES")"
      else
        expect_match "the allowed command ran" '200' "$(answer)"
      fi
    elif [[ $AGENT == codex && $MODE == docker ]]; then
      skip "the agent asks for permission" "Codex runs with externalSandbox and network on in a container: nothing to escalate"
    else
      fail "the agent asks for permission" "a pending permission within 120s" "$(j -c '{status, turns: .progress.turns, answer: .result.text}')"
    fi
    close_run "$run"
  fi
  if step 6.2 "Deny"; then
    permission_prompt denied.txt
    local run request
    run=$(start_run "$PERM_PROMPT" "$WS" ask "$PERM_EXTRA")
    if wait_run "$run" '(.waiting.permissions | length) > 0' 120; then
      request=$(j '.waiting.permissions[0].permission_id')
      api POST "/v1/runs/$run/permissions/$request/deny" '{"message":"no"}'
      expect "deny accepted" 202 "$CODE"
      wait_run "$run" '.progress.turns >= 1' 300
      if [[ $AGENT == claude ]]; then
        api GET "/v1/workspaces/$WS/files/download/denied.txt"
        expect "the denied write did not happen" not_found "$(j .code)"
      else
        pass "denied; answer: $(answer | head -c 120)"
      fi
    else
      fail "the agent asks for permission" "a pending permission within 120s" "$(j -c '{status, turns}')"
    fi
    close_run "$run"
  fi
  if step 6.3 "Workspace scope rule"; then
    permission_prompt scoped.txt
    local run request
    run=$(start_run "$PERM_PROMPT" "$WS" ask "$PERM_EXTRA")
    if wait_run "$run" '(.waiting.permissions | length) > 0' 120; then
      request=$(j '.waiting.permissions[0].permission_id')
      api POST "/v1/runs/$run/permissions/$request/allow" '{"scope":"workspace"}'
      wait_run "$run" '.progress.turns >= 1' 300
      api GET "/v1/workspaces/$WS/permissions"
      expect_match "the workspace rule is listed" '.+' "$(j '.workspace | join(",")')"
      api POST "/v1/workspaces/$WS/permissions/clear"
      api GET "/v1/workspaces/$WS/permissions"
      expect "clear removes it" 0 "$(j '.workspace | length')"
    else
      fail "the agent asks for permission" "a pending permission within 120s" "$(j -c '{status, turns}')"
    fi
    close_run "$run"
  fi
}

section_7() {
  if step 7.1 "Native control on an open run"; then
    ensure_workspace
    local run control
    run=$(start_run "Say hi." "$WS")
    wait_run "$run" '.progress.turns >= 1' 300
    if [[ $AGENT == claude ]]; then
      control='{"request":{"subtype":"get_context_usage"}}'
    else
      control='{"request":{"method":"mcpServerStatus/list","params":{}}}'
    fi
    api POST "/v1/runs/$run/native/control" "$control"
    expect "native response" true "$(j 'has("response")')"
    close_run "$run"
  fi
  if step 7.3 "Provider options"; then
    local own other
    if [[ $AGENT == claude ]]; then
      own='{"provider":{"name":"claude","max_turns":1}}'
      other='{"provider":{"name":"codex","reasoning_summary":"concise"}}'
    else
      own='{"provider":{"name":"codex","reasoning_summary":"concise"}}'
      other='{"provider":{"name":"claude","max_turns":1}}'
    fi
    api POST /v1/ask "$(body "Reply OK." "" "" "$own")"
    expect "own provider options accepted" succeeded "$(j .status)"
    api POST /v1/ask "$(body "Reply OK." "" "" "$other")"
    expect "the other provider's options are refused" "422 provider_mismatch" "$CODE $(j .code)"
  fi
}

section_8() {
  if step 8.1 "No token"; then
    expect "401" 401 "$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/agent")"
  fi
  if step 8.2 "Cross-origin browser request"; then
    local code
    code=$(curl -s -o "$RES" -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -H 'Origin: https://evil.example' \
      -H 'Content-Type: application/json' --data-binary '{}' "$URL/v1/workspaces/create")
    expect "refused" "403 cross_origin_request" "$code $(j .code)"
  fi
  if step 8.3 "Agent-written project config never loads"; then
    local ws
    ws=$(new_workspace)
    if [[ $AGENT == claude ]]; then
      ask 'Use the Write tool, not Bash, to create two files: ok.txt containing hi, then .mcp.json containing {"mcpServers":{}}.' "$ws"
      api GET "/v1/workspaces/$ws/files/download/ok.txt"
      if [[ $CODE != 200 ]]; then
        skip ".mcp.json refused" "the agent did not use the Write tool; rerun"
      else
        api GET "/v1/workspaces/$ws/files/download/.mcp.json"
        expect ".mcp.json was not written" not_found "$(j .code)"
      fi
    elif [[ $MODE == host ]]; then
      local marker
      marker=$(mktemp -u "${TMPDIR:-/tmp}/acceptance-marker.XXXXXX")
      api POST "/v1/workspaces/$ws/files/mkdir" '{"path":".codex","conflict":"error"}'
      upload "$ws" .codex/config.toml "$(printf '[mcp_servers.marker]\ncommand = "/usr/bin/touch"\nargs = ["%s"]\n' "$marker")"
      ask "Reply with exactly OK." "$ws"
      expect "project .codex/config.toml did not start its MCP server" absent "$([[ -e $marker ]] && echo present || echo absent)"
      rm -f "$marker"
    else
      skip "project config" "host mode only"
    fi
  fi
  if step 8.4 "Codex never records workspaces as trusted"; then
    if [[ $AGENT != codex || $MODE != host ]]; then
      skip "trust entries" "codex on the host only"
    else
      ensure_workspace
      expect "no trust entry for the workspace" 0 "$(grep -c "$WS" ~/.codex/config.toml 2>/dev/null || true)"
    fi
  fi
  if step 8.5 "The agent cannot reach the gateway"; then
    ensure_workspace
    if [[ $MODE == docker ]]; then
      # No CLI sandbox in a container: the agent reaches the gateway, but holds no token.
      ask "Run: curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/agent . Reply with only the number it printed." "$WS"
      expect_match "the agent gets 401 from the gateway in its container" 401 "$(answer)"
    else
      ask "Run: curl -s -m 5 -o /dev/null -w '%{http_code}' ${URL/localhost/127.0.0.1}/health . Reply with exactly REACHED if it printed 200, otherwise BLOCKED." "$WS"
      expect_match "loopback is blocked for the agent" BLOCKED "$(answer)"
    fi
  fi
  if step 8.6 "No writes outside the workspace"; then
    if [[ $MODE != host ]]; then
      skip "outside write" "host mode only"
    else
      ensure_workspace
      local target="$HOME/acceptance-outside-$$.txt"
      ask "Create the file $target containing x. Do not ask for approval." "$WS"
      expect "the file outside the workspace was not created" absent "$([[ -e $target ]] && echo present || echo absent)"
      rm -f "$target"
    fi
  fi
  if step 8.8 "Secrets in the gateway log"; then
    if [[ -n $DOCKER_AGENT ]]; then
      GW_LOG="$OUT/gateway.log"
      compose logs --no-color agent >"$GW_LOG" 2>&1
    fi
    if [[ -z $GW_LOG ]]; then
      skip "log scan" "start the gateway with -e or -d to capture its log"
    else
      local hits=0
      [[ -n $TOKEN ]] && hits=$(grep -cF "$TOKEN" "$GW_LOG")
      expect "the token is not logged" 0 "$hits"
      expect "no API keys are logged" 0 "$(grep -cE 'sk-[A-Za-z0-9_-]{16,}' "$GW_LOG")"
    fi
  fi
}

section_9() {
  if step 9.1 "Restart during a run"; then
    if ! can_restart; then
      skip "restart" "start the gateway with -e or -d so the script can restart it"
    else
      ensure_session
      local run children alive="" pid
      api POST "/v1/sessions/$SESSION/continue" '{"prompt":"Run: sleep 120, then reply DONE."}'
      run=$(j .run_id)
      sleep 10
      if [[ -n $GW_PID ]]; then
        children=$(descendants "$GW_PID" | tr '\n' ' ')
        stop_gateway
        sleep 2
        for pid in $children; do kill -0 "$pid" 2>/dev/null && alive+="$pid "; done
        expect "no agent process survives the gateway" "" "$alive"
        start_gateway
      else
        restart_gateway
      fi
      api GET "/v1/runs/$run"
      expect "the old run is gone" 404 "$CODE"
      api POST "/v1/sessions/$SESSION/continue" '{"prompt":"Do not run or resume any command. Just tell me the codeword."}'
      run=$(j .run_id)
      wait_run "$run" '.progress.turns >= 1' 300
      expect_match "the session continues after the restart" ZEBRA-42 "$(answer)"
      close_run "$run"
    fi
  fi
  if step 9.2 "Background task after the answer (claude)"; then
    if [[ $AGENT != claude ]]; then
      skip "background task" "claude only"
    else
      ask_user "This waits up to $QUERY_TIMEOUT s (QUERY_TIMEOUT). Run it?"
      if [[ $? == 1 ]]; then
        skip "background task" "declined"
      else
        ensure_workspace
        local started=$SECONDS
        ask "Start the shell command 'sleep 900' as a background task, then reply STARTED." "$WS"
        expect "the answer keeps its status" succeeded "$(j .status)"
        local took=$((SECONDS - started))
        expect "it returns within QUERY_TIMEOUT (took ${took}s)" true "$( ((took <= QUERY_TIMEOUT + 30)) && echo true || echo false)"
        if [[ $MODE == host ]]; then
          expect "no sleep 900 survives" "" "$(pgrep -f 'sleep 900' | tr '\n' ' ')"
        fi
      fi
    fi
  fi
  if step 9.3 "Long command output"; then
    ensure_workspace
    ask "Run seq 1 20000 and then reply with only the last line of its output." "$WS"
    expect "status" succeeded "$(j .status)"
    expect_match "answer" 20000 "$(answer)"
  fi
  if step 9.4 "Concurrency limit (parallel)"; then
    local i runs=() codes
    for i in $(seq 1 "$MAX_CONCURRENT"); do
      runs+=("$(start_run "Say hi." "$(new_workspace)")")
    done
    start_run "Say hi." "$(new_workspace)" >/dev/null
    codes="$CODE $(j .code)"
    expect "start $((MAX_CONCURRENT + 1)) is refused (MAX_CONCURRENT=$MAX_CONCURRENT)" "429 gateway_busy" "$codes"
    close_run "${runs[0]}"
    local extra
    extra=$(start_run "Say hi." "$(new_workspace)")
    expect_match "a freed slot is accepted again" '^[0-9a-f-]{36}$' "$extra"
    for i in "${runs[@]:1}" "$extra"; do api POST "/v1/runs/$i/close"; done
    for i in "${runs[@]:1}" "$extra"; do wait_run "$i" '.status != "running"' 60 >/dev/null; done
  fi
}

# ---------------------------------------------------------------- menu

SECTIONS=(
  "2  Agent and health"
  "3  One-shot questions"
  "4  Workspaces and files"
  "5  Runs and sessions (5.5 parallel watcher)"
  "6  Permissions"
  "7  Native controls and options"
  "8  Security"
  "9  Reliability (9.1 restart, 9.4 parallel limit)"
)
[[ -n $DOCKER_AGENT ]] && SECTIONS+=("10 Docker: restart keeps state, restricted egress, stop (10.1 build and 10.2 login always run)")
if [[ -z $SELECT ]]; then
  printf '%sGateway%s %s  %sagent%s %s  %smodel%s %s  %smode%s %s\n\n' "$B" "$N" "$URL" "$B" "$N" "$AGENT" "$B" "$N" "$MODEL" "$B" "$N" "$MODE"
  for line in "${SECTIONS[@]}"; do echo "  $line"; done
  echo
  read -r -p "Sections or steps to run (e.g. all | 2 4 | 5.5 6.1): " SELECT </dev/tty
  SELECT=${SELECT:-all}
fi

printf '\n%s%s on %s (%s), results in %s%s\n' "$B" "$AGENT" "$URL" "$MODE" "$OUT" "$N"
for section in 2 3 4 5 6 7 8 9; do
  "section_$section"
done
[[ -n $DOCKER_AGENT ]] && docker_steps

printf '\n%s%d passed, %d failed, %d skipped%s\n' "$B" "$PASSED" "$FAILED" "$SKIPPED" "$N"
echo "requests: $LOG"
((FAILED > 0)) && echo "findings: $FINDINGS"
((FAILED == 0))
