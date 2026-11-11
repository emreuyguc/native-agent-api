#!/bin/sh
# Check that each agent CLI pin (cli_version in docker/<agent>/Dockerfile) is at most 14 days
# behind the upstream releases: scripts/check-cli-releases.sh (or `make check-cli-releases`).
# It fails when a newer stable release is older than 14 days. A release younger than that waits,
# as Dependabot's cooldown does. GH_TOKEN, when set, raises the GitHub API rate limit.
set -eu
cd "$(dirname "$0")/.."
cooldown_days=14

page() {
  url="https://api.github.com/repos/$1/releases?per_page=30&page=$2"
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL --retry 3 --retry-all-errors -H 'Accept: application/vnd.github+json' -H "Authorization: Bearer $GH_TOKEN" "$url"
  else
    curl -fsSL --retry 3 --retry-all-errors -H 'Accept: application/vnd.github+json' "$url"
  fi
}

# releases <repository> <pinned tag>: the releases from the newest back to the pinned one, as one
# JSON array. Small pages: a Codex release lists many assets, and large pages time out.
releases() {
  all='[]'
  for n in 1 2 3 4 5 6 7 8 9 10; do
    raw=$(page "$1" "$n") || return 1
    json=$(printf '%s\n' "$raw" | jq -c '[.[] | {tag_name, draft, prerelease, published_at}]') || return 1
    all=$(printf '%s\n%s\n' "$all" "$json" | jq -cs 'add')
    if printf '%s\n' "$json" | jq -e --arg tag "$2" 'length == 0 or any(.[]; .tag_name == $tag)' >/dev/null; then
      printf '%s\n' "$all"
      return 0
    fi
  done
  echo "$2 is not among the latest 300 releases of $1" >&2
  return 1
}

# check <agent> <GitHub repository> <tag prefix>
behind=0
check() {
  pin=$(sed -n 's/.*cli_version=\([0-9][0-9.]*\);.*/\1/p' "docker/$1/Dockerfile")
  [ -n "$pin" ] || { echo "$1: no cli_version in docker/$1/Dockerfile" >&2; exit 1; }
  # A separate assignment: a failed download must fail the check, not read as "no releases".
  json=$(releases "$2" "$3$pin") || { echo "$1: cannot read the releases of $2" >&2; exit 1; }
  newer=$(printf '%s\n' "$json" | jq -r --arg pin "$pin" --arg prefix "$3" --argjson days "$cooldown_days" '
    ($pin | split(".") | map(tonumber)) as $current
    | [.[]
        | select((.draft or .prerelease) | not)
        | select(.tag_name | startswith($prefix))
        | (.tag_name | ltrimstr($prefix)) as $version
        | select($version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))
        | select(($version | split(".") | map(tonumber)) > $current)
        | select((.published_at | fromdate) <= now - $days * 86400)
        | "\($version) (\(.published_at[:10]))"]
    | if length == 0 then "" else "\(.[0]) and \(length - 1) older" end')
  if [ -n "$newer" ]; then
    echo "$1: $pin is behind releases older than $cooldown_days days: $newer"
    behind=1
  else
    echo "$1: $pin is current"
  fi
}

check claude anthropics/claude-code v
check codex openai/codex rust-v
[ "$behind" = 0 ] || {
  echo "Update cli_version and the checksums in docker/<agent>/Dockerfile (docs/development.md)."
  exit 1
}
