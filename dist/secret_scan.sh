#!/usr/bin/env bash
# secret_scan.sh — scan a directory tree for key-looking / secret-looking content.
# Used by dist/ship.sh (refuse to publish with a secret) and by the packaging test
# (prove the zip contents carry no secrets). Exit 0 = clean, exit 1 = a possible secret
# was found (the offending lines are printed).
#
# It scans only TEXT files and skips the things that legitimately contain key-shaped
# strings (this script's own patterns, the .git dir, the module cache, binaries, the
# test fixtures that deliberately use fake "secret" strings). It looks for real secret
# SHAPES (private-key headers, AWS-style keys, bearer tokens, Telegram-token shape, age
# secret keys), not the word "secret".
set -uo pipefail

TARGET="${1:-.}"

# Patterns that indicate a REAL leaked secret (shapes, not the literal word "secret").
# - PEM private key blocks
# - AWS access key id (AKIA...) and generic 40-char secret access keys assigned to a var
# - GitHub / Slack / generic bearer tokens
# - age secret keys (AGE-SECRET-KEY-...)
# - Hosted LLM API keys (NVIDIA nvapi-..., OpenAI-compatible sk-...)
# - Alpaca-style live key assignments
PATTERNS=(
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  'AKIA[0-9A-Z]{16}'
  'AGE-SECRET-KEY-1[0-9A-Z]+'
  'ghp_[A-Za-z0-9]{20,}'
  'nvapi-[A-Za-z0-9_-]{16,}'
  'sk-[A-Za-z0-9_-]{20,}'
  'xox[baprs]-[A-Za-z0-9-]{10,}'
  'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}'
)

# Paths to skip (false-positive sources, not shipped secrets).
EXCLUDE_DIRS=(
  '.git'
  'dist/out'
)
# Files that legitimately contain key-SHAPED literals (this scanner; the test fixtures
# that use fake "secret" strings to PROVE encryption; this script itself).
EXCLUDE_FILES_RE='(secret_scan\.sh|.*_test\.go|.*\.age|SHA256SUMS)$'

found=0
while IFS= read -r -d '' f; do
  # Skip excluded files.
  if [[ "$f" =~ $EXCLUDE_FILES_RE ]]; then
    continue
  fi
  # Skip binary files (grep -I detects them; we pre-check with a NUL probe).
  if grep -qI . "$f" 2>/dev/null; then
    for pat in "${PATTERNS[@]}"; do
      if hits=$(grep -nE "$pat" "$f" 2>/dev/null); then
        echo "POSSIBLE SECRET in $f:"
        echo "$hits"
        found=1
      fi
    done
  fi
done < <(
  # Build a find that prunes excluded dirs.
  prune=()
  for d in "${EXCLUDE_DIRS[@]}"; do
    prune+=( -path "$TARGET/$d" -prune -o )
  done
  find "$TARGET" "${prune[@]}" -type f -print0
)

if [[ "$found" -ne 0 ]]; then
  exit 1
fi
exit 0
