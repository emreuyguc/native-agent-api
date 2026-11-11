#!/bin/sh
# Compare the generated OpenAPI document with the one of another revision:
#   scripts/api-diff.sh <base revision, for example origin/main>
# It prints every change that can break an existing client and exits 1 when there is one.
set -eu
base=${1:?usage: scripts/api-diff.sh <base revision>}
cd "$(dirname "$0")/.."
work=$(mktemp -d)
trap 'git worktree remove --force "$work/base" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

git worktree add -q --detach "$work/base" "$base"
make -s -C "$work/base" build
"$work/base/bin/native-agent-api" openapi >"$work/base.yaml"
make -s build
bin/native-agent-api openapi >"$work/head.yaml"

go tool -modfile=tools/go.mod oasdiff breaking --fail-on ERR --format text \
  "$work/base.yaml" "$work/head.yaml"
