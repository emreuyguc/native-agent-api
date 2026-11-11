#!/bin/sh
# Smoke test of one Docker installation, run from anywhere:
#   docker/verify.sh <docker env file>
# It builds and starts the installation, checks the hardening and the tools the CLI needs, and,
# when COMPOSE_FILE includes compose.egress.yaml, that only allowlisted hosts are reachable. Then
# it stops the installation. Volumes stay.
# shellcheck disable=SC2016 # in_agent commands expand inside the container.
set -eu
env_file=${1:?usage: docker/verify.sh <docker env file>}
env_file=$(cd "$(dirname "$env_file")" && pwd)/$(basename "$env_file")
cd "$(dirname "$0")/.."
value() { sed -n "s/^$1=//p" "$env_file" | tail -n 1; }
agent=$(value CONTAINER_AGENT)
compose() { docker compose --env-file "$env_file" "$@"; }
in_agent() { compose exec -T agent sh -c "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

compose up -d --build --wait
trap 'compose stop >/dev/null 2>&1' EXIT

in_agent 'test "$(id -u)" = 65532' || fail "not running as uid 65532"
in_agent '! touch /usr/local/bin/x 2>/dev/null' || fail "root filesystem is writable"
in_agent 'command -v bash >/dev/null && command -v git >/dev/null' || fail "bash or git missing"
in_agent 'printf "#!/bin/sh\n" >/tmp/.verify && chmod +x /tmp/.verify && /tmp/.verify && rm /tmp/.verify' ||
  fail "/tmp is not executable"
in_agent "command -v $agent >/dev/null && $agent --version" || fail "$agent CLI missing"
in_agent 'test "$NATIVE_AGENT_AGENT" = '"$agent" || fail "NATIVE_AGENT_AGENT is not $agent"
in_agent 'env | grep -q "^CONTAINER_"' && fail "a Docker env setting reached the container"
in_agent 'native-agent-api healthcheck' || fail "healthcheck failed"

egress=""
case $(value COMPOSE_FILE) in *compose.egress.yaml*) egress=" (restricted egress)" ;; esac
if [ -n "$egress" ]; then
  case "$agent" in claude) allowed=api.anthropic.com ;; codex) allowed=api.openai.com ;; esac
  in_agent 'env -u https_proxy -u HTTPS_PROXY wget -q -T 5 -O /dev/null https://example.com 2>/dev/null' &&
    fail "direct egress is open"
  in_agent 'wget -q -T 8 -O /dev/null https://example.com 2>&1 | grep -q Filtered' ||
    fail "the proxy did not refuse a host outside the allowlist"
  in_agent "wget -T 8 -S -O /dev/null https://$allowed/ 2>&1 | grep -q 'HTTP/'" ||
    fail "the proxy did not reach $allowed"
fi
echo "OK: $(value COMPOSE_PROJECT_NAME)$egress"
