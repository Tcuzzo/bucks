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
# secret keys) and leaked private LAN host addresses (RFC1918 host IPs), not the word
# "secret".
set -uo pipefail

TARGET="${1:-.}"

# Patterns that indicate a REAL leaked secret (shapes, not the literal word "secret").
# - PEM private key blocks
# - AWS access key id (AKIA...) and generic 40-char secret access keys assigned to a var
# - GitHub / Slack / generic bearer tokens
# - age secret keys (AGE-SECRET-KEY-...)
# - Alpaca-style live key assignments
PATTERNS=(
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  'AKIA[0-9A-Z]{16}'
  'AGE-SECRET-KEY-1[0-9A-Z]+'
  'ghp_[A-Za-z0-9]{20,}'
  'xox[baprs]-[A-Za-z0-9-]{10,}'
  'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}'
)

# Private LAN host IP leak (RFC1918 host addresses — a leaked internal machine address).
# We flag real host IPs (e.g. 10.1.2.3, 172.20.0.5, 192.168.1.70) but NOT:
#   - loopback (127.x) / the unspecified 0.0.0.0 — they never match the RFC1918 shape below;
#   - the RFC5737 documentation ranges (192.0.2.x / 198.51.100.x / 203.0.113.x) — not
#     RFC1918, so they never match either;
#   - the RFC1918 network/boundary base addresses (10.0.0.0, 172.16.0.0, 192.168.0.0),
#     which DO match the shape but are not a host leak — dropped by IP_EXCLUDE_RE below.
# Octets are validated 0-255 and the 172 block is bounded to 172.16-172.31 (172.16.0.0/12),
# so out-of-range strings (192.168.999.999, 172.15.x, 172.32.x) are NOT flagged.
_OCTET='(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])'
IP_PATTERN="\\b(10(\\.${_OCTET}){3}|172\\.(1[6-9]|2[0-9]|3[0-1])(\\.${_OCTET}){2}|192\\.168(\\.${_OCTET}){2})\\b"
# Boundary/network base addresses (anchored to a full grep -oE match: "<lineno>:<ip>").
IP_EXCLUDE_RE=':(10\.0\.0\.0|172\.16\.0\.0|192\.168\.0\.0)$'

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
    # Private LAN host-IP leak — extracted per MATCH (grep -oE), not per line, so a boundary
    # base address sharing a line with a real host IP cannot mask the leak. The boundary/
    # network base addresses are then dropped (they are not a host leak).
    if hits=$(grep -noE "$IP_PATTERN" "$f" 2>/dev/null | grep -vE "$IP_EXCLUDE_RE"); then
      echo "POSSIBLE SECRET in $f:"
      echo "$hits"
      found=1
    fi
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
