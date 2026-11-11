#!/bin/sh
# Interactive setup of one native-agent-api installation: scripts/setup.sh (or `make setup`).
# It asks how to run the gateway and for its settings, writes the env files, and can start it.
# Every value it writes is shown first; press Enter to accept a suggestion. It never reads or
# changes the native CLI profile, and it never overwrites a file without asking.
set -eu

cd "$(dirname "$0")/.."
ROOT=$(pwd)

if [ -t 1 ]; then
  B=$(printf '\033[1m') D=$(printf '\033[2m') G=$(printf '\033[32m') R=$(printf '\033[31m') N=$(printf '\033[0m')
else
  B="" D="" G="" R="" N=""
fi

die() { printf '%s%s%s\n' "$R" "$*" "$N" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
title() { printf '\n%s%s%s\n' "$B" "$*" "$N"; }

# ask VARIABLE "Question" "suggestion": read one line; an empty answer takes the suggestion.
ask() {
  if [ -n "$3" ]; then
    printf '%s %s[%s]%s: ' "$2" "$D" "$3" "$N"
  else
    printf '%s: ' "$2"
  fi
  IFS= read -r answer || die "no answer for: $2"
  [ -n "$answer" ] || answer=$3
  eval "$1=\$answer"
}

# choose VARIABLE "Question" "value:label" ...: numbered choice; the first option is suggested.
choose() {
  variable=$1 question=$2
  shift 2
  say "$question"
  index=0
  for option in "$@"; do
    index=$((index + 1))
    printf '  %s) %s\n' "$index" "${option#*:}"
  done
  while :; do
    ask picked "Choice" 1
    index=0
    for option in "$@"; do
      index=$((index + 1))
      if [ "$picked" = "$index" ]; then
        eval "$variable=\${option%%:*}"
        return 0
      fi
    done
    say "Enter a number from 1 to $#."
  done
}

# confirm "Question" y|n: true for yes; the second argument is the suggestion.
confirm() {
  while :; do
    ask reply "$1 (y/n)" "$2"
    case $reply in
      y | Y | yes) return 0 ;;
      n | N | no) return 1 ;;
    esac
  done
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required${2:+: $2}"; }

# set_kv FILE KEY VALUE: replace KEY=... in FILE, keeping comments and the file mode.
set_kv() {
  grep -q "^$2=" "$1" || die "$1 has no $2 line"
  tmp=$(mktemp)
  awk -v k="$2" -v v="$3" 'index($0, k "=") == 1 { print k "=" v; next } { print }' "$1" >"$tmp"
  cat "$tmp" >"$1"
  rm -f "$tmp"
}

# new_file SOURCE TARGET: copy the example with mode 0600; ask before replacing an existing file.
new_file() {
  if [ -e "$2" ]; then
    confirm "$2 exists. Replace it?" n || die "Stopped: $2 is unchanged. Run again with another name."
  fi
  (umask 077 && cp "$1" "$2")
  chmod 600 "$2"
}

# Settings docker/compose.yaml fixes for a container; its application env file leaves them out.
CONTAINER_FIXED='AGENT|SANDBOX_MODE|LISTEN|DATA_DIR|ALLOWED_ROOTS|PROFILE_DIR'

valid_port() {
  case $1 in '' | *[!0-9]*) return 1 ;; esac
  [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf %s "$1" | sha256sum | cut -d' ' -f1
  else
    printf %s "$1" | shasum -a 256 | cut -d' ' -f1
  fi
}

# ---------------------------------------------------------------- questions

title "native-agent-api setup"
say "One installation serves one agent. Run this again for another one."

title "1. How to run"
choose MODE "Run the gateway:" \
  "docker:In a Docker container (needs Docker with Compose)" \
  "build:On this machine, built from this source (needs Go)" \
  "binary:On this machine, with an installed native-agent-api binary"

case $MODE in
  docker)
    need docker "install Docker with Compose v2"
    docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
    git rev-parse HEAD >/dev/null 2>&1 || die "the image reads its version from git: run this in a git checkout"
    ;;
  build) need go "see go.mod for the version" ;;
  binary) need native-agent-api "install it with: go install ./cmd/native-agent-api" ;;
esac
need openssl "it creates the client token"
command -v sha256sum >/dev/null 2>&1 || need shasum

title "2. Agent"
choose AGENT "Serve:" "claude:Claude Code" "codex:Codex CLI"
if [ "$MODE" != docker ]; then
  command -v "$AGENT" >/dev/null 2>&1 || die "the $AGENT CLI is not on PATH; install it first"
fi

title "3. Settings"
if [ "$MODE" = docker ]; then
  while :; do
    ask NAME "Installation name (files docker/<name>.env, volumes native-agent-api-<name>-*)" "$AGENT"
    case $NAME in
      '' | *[!a-z0-9-]*) say "Use lower-case letters, digits and hyphens." ;;
      *) break ;;
    esac
  done
  DOCKER_ENV=docker/$NAME.env
  APP_ENV=docker/$NAME.app.env
  suggested_port=8701
  [ "$AGENT" = codex ] && suggested_port=8702
  LISTEN=0.0.0.0:8080
  choose PUBLISH_ADDR "Publish the port on:" \
    "127.0.0.1:This machine only" \
    "0.0.0.0:Every network interface"
  if confirm "Restrict the agent's network to the hosts in docker/$AGENT/egress-allow.txt?" n; then
    COMPOSE_FILES=docker/compose.yaml:docker/compose.egress.yaml
  else
    COMPOSE_FILES=docker/compose.yaml
  fi
else
  ask APP_ENV "Env file to write" .env
  suggested_port=8080
  choose LISTEN_ADDR "Listen on:" \
    "127.0.0.1:This machine only" \
    "0.0.0.0:Every network interface (a token is required)"
  while :; do
    ask DATA_DIR "Data directory (absolute; workspaces and session records)" "$HOME/.local/share/native-agent-api/$AGENT"
    case $DATA_DIR in /?*) break ;; *) say "Enter an absolute path." ;; esac
  done
  ask ALLOWED_ROOTS "Host directories clients may attach (comma-separated absolute paths; empty: none)" ""
fi
while :; do
  ask PORT "Port" "$suggested_port"
  valid_port "$PORT" && break
  say "Enter a port from 1 to 65535."
done
[ "$MODE" = docker ] || LISTEN=$LISTEN_ADDR:$PORT

TOKEN="" DIGEST=""
case $LISTEN in
  127.0.0.1:*)
    confirm "Protect the API with a client token?" y && TOKEN=$(openssl rand -hex 32)
    ;;
  *)
    say "The listener is reachable beyond loopback, so a client token is required."
    TOKEN=$(openssl rand -hex 32)
    ;;
esac
[ -z "$TOKEN" ] || DIGEST=$(sha256 "$TOKEN")

# ---------------------------------------------------------------- files

title "4. Files"
new_file .env.example "$APP_ENV"
if [ "$MODE" = docker ]; then
  {
    say "# Application settings of the $NAME container. /.env.example describes each one."
    say "# docker/compose.yaml sets the agent, sandbox mode, listener and directories."
    grep '^NATIVE_AGENT_' .env.example | grep -vE "^NATIVE_AGENT_($CONTAINER_FIXED)="
  } >"$APP_ENV"
  set_kv "$APP_ENV" NATIVE_AGENT_TOKEN_SHA256 "$DIGEST"

  new_file docker/.env.example "$DOCKER_ENV"
  set_kv "$DOCKER_ENV" COMPOSE_PROJECT_NAME "native-agent-api-$NAME"
  set_kv "$DOCKER_ENV" COMPOSE_FILE "$COMPOSE_FILES"
  set_kv "$DOCKER_ENV" CONTAINER_AGENT "$AGENT"
  set_kv "$DOCKER_ENV" CONTAINER_APP_ENV_FILE "./$NAME.app.env"
  set_kv "$DOCKER_ENV" CONTAINER_PUBLISH_ADDR "$PUBLISH_ADDR"
  set_kv "$DOCKER_ENV" CONTAINER_PUBLISH_PORT "$PORT"
  say "Wrote $DOCKER_ENV and $APP_ENV."
  URL_HOST=$PUBLISH_ADDR
else
  set_kv "$APP_ENV" NATIVE_AGENT_AGENT "$AGENT"
  set_kv "$APP_ENV" NATIVE_AGENT_LISTEN "$LISTEN"
  set_kv "$APP_ENV" NATIVE_AGENT_TOKEN_SHA256 "$DIGEST"
  set_kv "$APP_ENV" NATIVE_AGENT_SANDBOX_MODE cli
  set_kv "$APP_ENV" NATIVE_AGENT_DATA_DIR "$DATA_DIR"
  set_kv "$APP_ENV" NATIVE_AGENT_ALLOWED_ROOTS "$ALLOWED_ROOTS"
  set_kv "$APP_ENV" NATIVE_AGENT_PROFILE_DIR native
  say "Wrote $APP_ENV."
  URL_HOST=$LISTEN_ADDR
fi
[ "$URL_HOST" != 0.0.0.0 ] || URL_HOST=127.0.0.1
URL=http://$URL_HOST:$PORT
say "Every other setting has the value from the example; edit the file to change one."

# ---------------------------------------------------------------- next steps

case $MODE in
  docker) START="docker compose --env-file $DOCKER_ENV up -d --build --wait" ;;
  build) START="make run ENV_FILE=$APP_ENV" ;;
  binary) START="native-agent-api serve --env-file $APP_ENV" ;;
esac

title "5. Next steps"
if [ -n "$TOKEN" ]; then
  say "Client token, shown once. The env file holds only its SHA-256 digest. Keep it:"
  printf '\n  %sexport TOKEN=%s%s\n\n' "$G" "$TOKEN" "$N"
  AUTH=" -H \"Authorization: Bearer \$TOKEN\""
else
  AUTH=""
  say "No client token: any process on this machine can use the API."
fi
case $AGENT in claude) method=claudeai ;; codex) method=chatgpt_device_code ;; esac
cat <<EOF
  $START
  curl -s$AUTH $URL/v1/agent/status | jq .logged_in

If logged_in is false, log in through the API (docs/api.md, "Agent endpoints"):
  curl -s -X POST$AUTH -H 'Content-Type: application/json' \\
    -d '{"method":"$method"}' $URL/v1/agent/login/start | jq .step

API reference: $URL/docs
EOF

if [ "$MODE" = docker ]; then
  question="Build the image and start the container now?"
else
  question="Start the gateway now? It runs in this terminal until you stop it."
fi
if confirm "$question" y; then
  say "$D$START$N"
  case $MODE in
    docker) exec docker compose --env-file "$DOCKER_ENV" up -d --build --wait ;;
    build) exec make run ENV_FILE="$APP_ENV" ;;
    binary) exec native-agent-api serve --env-file "$APP_ENV" ;;
  esac
fi
