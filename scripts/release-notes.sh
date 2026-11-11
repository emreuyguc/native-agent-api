#!/bin/sh
# Print the CHANGELOG.md section of one release: scripts/release-notes.sh v0.2.0
# It fails when the section is missing or empty, so a tag without release notes is not released.
set -eu
tag=${1:?usage: scripts/release-notes.sh <tag, for example v0.2.0>}
version=${tag#v}
cd "$(dirname "$0")/.."
notes=$(awk -v v="$version" '
  index($0, "## [" v "]") == 1 { found = 1; next }
  found && /^## \[/ { exit }
  found && /^\[[^]]+\]: / { exit }
  found { print }
' CHANGELOG.md | sed -e '/./,$!d')
[ -n "$notes" ] || { echo "CHANGELOG.md has no section for $version" >&2; exit 1; }
printf '%s\n' "$notes"
