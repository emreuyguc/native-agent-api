#!/bin/sh
# Smoke test of one Docker service with the configuration in docker/.env:
#   docker/verify.sh <claude|codex> [--egress]
# It builds and starts the service, checks the hardening and the tools the CLI needs, with
# --egress also that only allowlisted hosts are reachable, then stops the service. Volumes stay.
# shellcheck disable=SC2016 # in_agent commands expand inside the container.
set -eu
agent=${1:?usage: docker/verify.sh <claude|codex> [--egress]}
egress=${2:-}
cd "$(dirname "$0")/.."
files="-f docker/compose.yaml -f docker/compose.$agent.yaml"
[ "$egress" = --egress ] && files="$files -f docker/compose.$agent.egress.yaml"
# shellcheck disable=SC2086 # $files holds several -f options; the paths contain no spaces.
compose() { docker compose --env-file docker/.env $files --profile "$agent" "$@"; }
in_agent() { compose exec -T "$agent" sh -c "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

compose up -d --build --wait
trap 'compose stop >/dev/null 2>&1' EXIT

in_agent 'test "$(id -u)" = 65532' || fail "not running as uid 65532"
in_agent '! touch /usr/local/bin/x 2>/dev/null' || fail "root filesystem is writable"
in_agent 'command -v bash >/dev/null && command -v git >/dev/null' || fail "bash or git missing"
in_agent 'printf "#!/bin/sh\n" >/tmp/.verify && chmod +x /tmp/.verify && /tmp/.verify && rm /tmp/.verify' ||
  fail "/tmp is not executable"
in_agent "command -v $agent >/dev/null && $agent --version" || fail "$agent CLI missing"
in_agent 'native-agent-api healthcheck' || fail "healthcheck failed"

if [ "$egress" = --egress ]; then
  case "$agent" in claude) allowed=api.anthropic.com ;; codex) allowed=api.openai.com ;; esac
  in_agent 'env -u https_proxy -u HTTPS_PROXY wget -q -T 5 -O /dev/null https://example.com 2>/dev/null' &&
    fail "direct egress is open"
  in_agent 'wget -q -T 8 -O /dev/null https://example.com 2>&1 | grep -q Filtered' ||
    fail "the proxy did not refuse a host outside the allowlist"
  in_agent "wget -T 8 -S -O /dev/null https://$allowed/ 2>&1 | grep -q 'HTTP/'" ||
    fail "the proxy did not reach $allowed"
fi
echo "OK: $agent docker${egress:+ ($egress)}"
