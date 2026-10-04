#!/usr/bin/env bash
set -euo pipefail

# Include local edits without committing: gorelease otherwise reads Git HEAD
# and rejects a dirty worktree. Only repository files enter the temporary module.
cd "$(git rev-parse --show-toplevel)"
snapshot=$(mktemp -d "${TMPDIR:-/tmp}/aiusage-core-api.XXXXXXXX")
trap 'rm -rf -- "$snapshot"' EXIT
git ls-files --cached --others --exclude-standard -z |
  tar --null -T - -cf - | tar -C "$snapshot" -xf -
cd "$snapshot"

# gorelease permits incompatibilities when the BASE is v0, even with a v1
# candidate. Reject its pinned-format incompatible section explicitly as well
# as respecting tool errors. This checks our stronger compatibility contract.
GOWORK=off go run golang.org/x/exp/cmd/gorelease@v0.0.0-20260908205506-85c1c2202aba \
  -base="${1:-v0.1.0}" -version="${2:-v1.0.0}" | tee "$snapshot/api-report.txt"
if grep -q '^## incompatible changes$' "$snapshot/api-report.txt"; then
  echo 'Public API compatibility check failed: incompatible changes from the released baseline.' >&2
  exit 1
fi
