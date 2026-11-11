#!/bin/sh
# Smoke test of one agent image without a login: docker/test-image.sh claude|codex
# (or `make test-image AGENT=…`). It writes a throwaway installation to a temporary directory,
# runs docker/verify.sh on it, and then removes its containers and volumes. The image stays as
# native-agent-api-imagetest-<agent>:local for `make scan-images`.
# Its own project and image name keep your installations and their images untouched.
set -eu
agent=${1:?usage: docker/test-image.sh claude|codex}
case $agent in claude | codex) ;; *) echo "unknown agent: $agent" >&2; exit 2 ;; esac
cd "$(dirname "$0")/.."

dir=$(mktemp -d)
project=native-agent-api-imagetest-$agent
cleanup() {
  docker compose --env-file "$dir/docker.env" down --volumes >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT

# The settings scripts/setup.sh writes for a Docker installation. No client logs in, so the
# token digest is random and no token exists.
{
  grep '^NATIVE_AGENT_' .env.example |
    grep -vE '^NATIVE_AGENT_(AGENT|SANDBOX_MODE|LISTEN|DATA_DIR|ALLOWED_ROOTS|PROFILE_DIR|TOKEN_SHA256)='
  echo "NATIVE_AGENT_TOKEN_SHA256=$(openssl rand -hex 32)"
} >"$dir/app.env"
printf 'services:\n  agent:\n    image: %s:local\n' "$project" >"$dir/compose.override.yaml"
cat >"$dir/docker.env" <<EOF
COMPOSE_PROJECT_NAME=$project
COMPOSE_FILE=docker/compose.yaml:$dir/compose.override.yaml
CONTAINER_AGENT=$agent
CONTAINER_APP_ENV_FILE=$dir/app.env
CONTAINER_PUBLISH_ADDR=127.0.0.1
CONTAINER_PUBLISH_PORT=0
CONTAINER_MEMORY=1g
CONTAINER_CPUS=1.0
EOF

# A fresh build: a cached layer would keep old Alpine packages from the scan.
docker compose --env-file "$dir/docker.env" build --pull --no-cache
docker/verify.sh "$dir/docker.env"
