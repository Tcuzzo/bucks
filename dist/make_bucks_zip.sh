#!/usr/bin/env bash
# make_bucks_zip.sh — build BUCKS's single static binary for every shipped OS/arch and
# bundle each into a release zip. This is the portable packaging path used when
# GoReleaser is not installed (the .goreleaser.yaml is the equivalent for CI where it
# is). It produces REPRODUCIBLE builds: CGO_ENABLED=0, -trimpath, and fixed ldflags so a
# user can verify the binary that will hold their keys.
#
# Each zip contains: the binary + LICENSE + NOTICE + README.md + the guided first-run
# installers (install.sh for Linux/macOS, install.ps1 for Windows). NO secrets ever.
#
# Usage:
#   dist/make_bucks_zip.sh [OUT_DIR]
# OUT_DIR defaults to ./dist/out. Set VERSION to stamp a version (default: dev).
set -euo pipefail

# --- locate the module root (the dir holding go.mod), regardless of cwd ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

OUT_DIR="${1:-$ROOT/dist/out}"
VERSION="${VERSION:-dev}"
# Reproducible build date: honor SOURCE_DATE_EPOCH if set, else a fixed stamp (NOT
# wall-clock, so two builds of the same source produce the same binary).
BUILD_DATE="${SOURCE_DATE_EPOCH:-0}"

# Pick the go toolchain. Prefer a real (non-snap-wrapper) go if GO is exported.
GO="${GO:-go}"

# Fixed ldflags: strip debug info (-s -w) for a smaller, deterministic binary and stamp
# the version. -trimpath removes local filesystem paths so the build is reproducible.
# The updater.Version stamp is what `bucks version` reports and what the self-updater
# compares against the latest GitHub Release tag — so a shipped binary knows its own
# version and can tell when a newer release is out.
LDFLAGS="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE} -X 'bucks/internal/updater.Version=${VERSION}'"

# Targets we ship. (os arch) pairs.
TARGETS=(
  "linux amd64"
  "linux arm64"
  "windows amd64"
  "darwin amd64"
  "darwin arm64"
)

echo "BUCKS packaging — version=${VERSION} out=${OUT_DIR}"
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

# Sanity: the attribution + doc files MUST exist (we never ship without them).
for f in LICENSE NOTICE README.md; do
  if [[ ! -f "$ROOT/$f" ]]; then
    echo "FATAL: required ship file missing: $f" >&2
    exit 1
  fi
done

for t in "${TARGETS[@]}"; do
  set -- $t
  GOOS="$1"; GOARCH="$2"
  bin="bucks"
  [[ "$GOOS" == "windows" ]] && bin="bucks.exe"

  stage="$OUT_DIR/BUCKS_${GOOS}_${GOARCH}"
  mkdir -p "$stage"

  echo "  building ${GOOS}/${GOARCH} ..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    "$GO" build -trimpath -ldflags "$LDFLAGS" -o "$stage/$bin" ./cmd/bucks

  # Bundle the docs + installers alongside the binary.
  cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$ROOT/README.md" "$stage/"
  cp "$SCRIPT_DIR/install.sh" "$stage/install.sh"
  cp "$SCRIPT_DIR/install.ps1" "$stage/install.ps1"
  chmod +x "$stage/install.sh" "$stage/$bin"

  # Zip it (store paths relative to OUT_DIR so the zip has a clean top-level dir).
  zip_name="BUCKS_${GOOS}_${GOARCH}.zip"
  ( cd "$OUT_DIR" && zip -q -r -X "$zip_name" "BUCKS_${GOOS}_${GOARCH}" )
  echo "  -> $OUT_DIR/$zip_name"

  # Clean the staging dir; keep only the zips.
  rm -rf "$stage"
done

# Checksums so users can verify what they downloaded.
( cd "$OUT_DIR" && for z in BUCKS_*.zip; do
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$z"; else shasum -a 256 "$z"; fi
  done > SHA256SUMS )

echo "Done. Artifacts in $OUT_DIR:"
ls -1 "$OUT_DIR"
