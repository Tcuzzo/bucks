#!/usr/bin/env bash
# ship.sh — OPERATOR-GATED publish of the BUCKS MIT repo to GitHub.
#
# ============================ READ THIS FIRST ============================
# This script does NOTHING destructive and pushes NOTHING on its own. A real push
# requires the OPERATOR to supply:
#   1. their GitHub repository URL  (BUCKS_REMOTE, e.g. git@github.com:you/bucks.git)
#   2. their own git/GitHub credentials (SSH key or a gh-authenticated session)
#   3. an explicit confirmation flag  (--confirm)
#
# Without --confirm it runs in DRY-RUN mode: it verifies the tree is clean and
# secret-free and PRINTS exactly what it WOULD do, then exits. This is by design — the
# operator owns the moment of publication (CLAUDE.md: destructive/irreversible is gated;
# a public push is irreversible). Claude/CI must NOT auto-run a real push.
# ========================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

CONFIRM=0
for a in "$@"; do
  [[ "$a" == "--confirm" ]] && CONFIRM=1
done

REMOTE="${BUCKS_REMOTE:-}"
BRANCH="${BUCKS_BRANCH:-main}"

echo "== BUCKS ship preflight =="

# 1. The ship files must exist.
for f in LICENSE NOTICE README.md; do
  [[ -f "$ROOT/$f" ]] || { echo "FATAL: missing ship file: $f" >&2; exit 1; }
done
echo "  ship files present: LICENSE, NOTICE, README.md"

# 2. License gate must be green (no copyleft anywhere in the tree).
GO="${GO:-go}"
echo "  running the license gate ..."
if ! "$GO" test ./internal/license/ -run 'TestScanCuratedRealTreePasses|TestEveryGoModRequireIsCurated' -count=1 >/dev/null 2>&1; then
  echo "FATAL: license gate failed — refusing to ship. Run: go test ./internal/license/ -v" >&2
  exit 1
fi
echo "  license gate PASS (MIT-clean tree)"

# 3. Secret scan: refuse to ship if anything key-looking is tracked in the tree.
echo "  scanning for committed secrets ..."
if "$SCRIPT_DIR/secret_scan.sh" "$ROOT"; then
  echo "  secret scan PASS (no key-looking content)"
else
  echo "FATAL: possible secret found in the tree — refusing to ship. See output above." >&2
  exit 1
fi

# 4. The actual push — gated.
if [[ "$CONFIRM" -ne 1 ]]; then
  cat <<EOF

DRY RUN (no push performed). Preflight is GREEN.

To publish, the OPERATOR runs (with their own remote + credentials):

    export BUCKS_REMOTE="git@github.com:<operator>/bucks.git"
    dist/ship.sh --confirm

That would: ensure remote 'origin' = \$BUCKS_REMOTE, then
    git push origin ${BRANCH}

This script will NOT do that without --confirm AND a BUCKS_REMOTE you provide.
EOF
  exit 0
fi

# --confirm path: still requires the operator-provided remote. We do not invent one.
if [[ -z "$REMOTE" ]]; then
  echo "FATAL: --confirm given but BUCKS_REMOTE is not set. Export your GitHub repo URL first." >&2
  exit 1
fi

echo "  operator-confirmed publish to: $REMOTE (branch $BRANCH)"
# Configure origin to the operator's remote (idempotent).
if git remote get-url origin >/dev/null 2>&1; then
  git remote set-url origin "$REMOTE"
else
  git remote add origin "$REMOTE"
fi
git push origin "$BRANCH"
echo "  pushed. BUCKS is live at $REMOTE"
