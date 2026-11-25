#!/usr/bin/env bash
# Print the CHANGELOG.md section for a release tag (e.g. v1.0.0). Fails if there is none.
set -euo pipefail
version="${1#v}"
notes=$(awk -v v="$version" '
  { sub(/\r$/, "") }
  /^(```|~~~)/ { fence = !fence }
  !fence && /^## / { if (on) exit; on = ($2 == v || $2 == "[" v "]"); next }
  on { print }
' CHANGELOG.md)
if [ -z "$(printf %s "$notes" | tr -d '[:space:]')" ]; then
  echo "No CHANGELOG.md entry for $version" >&2
  exit 1
fi
printf '%s\n' "$notes"
