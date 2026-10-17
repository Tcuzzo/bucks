#!/usr/bin/env sh
# install.sh — BUCKS remote installer / updater (Linux + macOS).
#
# One command installs BUCKS if it's absent and updates it if it's present —
# no manual download, no unzip, no reinstall churn. It pulls the latest release
# straight from GitHub, VERIFIES the download against the published SHA256SUMS
# (it ABORTS on any mismatch — an unverified binary is never installed), and
# drops the `bucks` binary into a user-local bin dir. It never uses sudo and
# never writes outside your home directory.
#
#   curl -fsSL https://raw.githubusercontent.com/Tcuzzo/bucks/main/install.sh | bash
#
# Env:
#   BUCKS_INSTALL_DIR   where to put the binary (default: $HOME/.local/bin)
set -eu

REPO="Tcuzzo/bucks"
BASE="https://github.com/${REPO}/releases/latest/download"

# ---- clean header (no ASCII animal) -----------------------------------------
echo "BUCKS installer"
echo "Paper trading only. bucks cannot place real-money orders."
echo

# ---- detect OS + arch -------------------------------------------------------
os_raw="$(uname -s)"
case "$os_raw" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *)
    echo "BUCKS: unsupported operating system '$os_raw'." >&2
    echo "This installer supports Linux and macOS. On Windows use install.ps1." >&2
    exit 1
    ;;
esac

arch_raw="$(uname -m)"
case "$arch_raw" in
  x86_64|amd64)        ARCH="amd64" ;;
  aarch64|arm64)       ARCH="arm64" ;;
  *)
    echo "BUCKS: unsupported CPU architecture '$arch_raw'." >&2
    echo "Supported: x86_64 (amd64) and aarch64/arm64." >&2
    exit 1
    ;;
esac

ASSET="BUCKS_${OS}_${ARCH}.zip"
EXTRACT_DIR="BUCKS_${OS}_${ARCH}"

# ---- required tools ---------------------------------------------------------
# Downloader: prefer curl, fall back to wget.
if command -v curl >/dev/null 2>&1; then
  DL="curl"
elif command -v wget >/dev/null 2>&1; then
  DL="wget"
else
  echo "BUCKS: need 'curl' or 'wget' to download, but neither is installed." >&2
  exit 1
fi

if ! command -v unzip >/dev/null 2>&1; then
  echo "BUCKS: 'unzip' is required but not installed." >&2
  echo "Install it first (e.g. 'sudo apt-get install unzip' or 'brew install unzip')." >&2
  exit 1
fi

# sha256 tool: sha256sum on Linux, shasum -a 256 on macOS.
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  echo "BUCKS: need 'sha256sum' or 'shasum' to verify the download, but neither is installed." >&2
  exit 1
fi

# ---- work dir with guaranteed cleanup ---------------------------------------
WORK="$(mktemp -d "${TMPDIR:-/tmp}/bucks-install.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM HUP

# download <url> <dest>
download() {
  if [ "$DL" = "curl" ]; then
    curl -fsSL -o "$2" "$1"
  else
    wget -q -O "$2" "$1"
  fi
}

echo "Detected: ${OS}/${ARCH}"
echo "Fetching the latest BUCKS release..."

# ---- download asset + checksums ---------------------------------------------
if ! download "${BASE}/${ASSET}" "${WORK}/${ASSET}"; then
  echo "BUCKS: failed to download ${ASSET} from the latest release." >&2
  echo "Check your connection, or grab a zip manually from:" >&2
  echo "  https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

if ! download "${BASE}/SHA256SUMS" "${WORK}/SHA256SUMS"; then
  echo "BUCKS: failed to download SHA256SUMS — cannot verify the binary, aborting." >&2
  exit 1
fi

# ---- VERIFY (security core) -------------------------------------------------
# Pull the expected hash for our asset out of SHA256SUMS.
EXPECTED="$(awk -v f="$ASSET" '$2 == f {print $1}' "${WORK}/SHA256SUMS" | head -n1)"
if [ -z "$EXPECTED" ]; then
  echo "BUCKS: no checksum entry for ${ASSET} in SHA256SUMS — refusing to install." >&2
  exit 1
fi

ACTUAL="$(sha256_of "${WORK}/${ASSET}")"
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "BUCKS: CHECKSUM MISMATCH for ${ASSET} — refusing to install." >&2
  echo "  expected: ${EXPECTED}" >&2
  echo "  actual:   ${ACTUAL}" >&2
  echo "The download is corrupt or has been tampered with. Nothing was installed." >&2
  exit 1
fi
echo "Checksum verified (sha256)."

# ---- unzip + locate binary --------------------------------------------------
if ! unzip -q -o "${WORK}/${ASSET}" -d "${WORK}/unpacked"; then
  echo "BUCKS: failed to unzip ${ASSET}." >&2
  exit 1
fi

BIN_SRC="${WORK}/unpacked/${EXTRACT_DIR}/bucks"
if [ ! -f "$BIN_SRC" ]; then
  # Fallback: locate it anywhere in the unpacked tree.
  BIN_SRC="$(find "${WORK}/unpacked" -type f -name bucks | head -n1)"
fi
if [ -z "${BIN_SRC:-}" ] || [ ! -f "$BIN_SRC" ]; then
  echo "BUCKS: could not find the 'bucks' binary inside ${ASSET}." >&2
  exit 1
fi

# ---- install (user-local, never sudo) ---------------------------------------
DEST="${BUCKS_INSTALL_DIR:-$HOME/.local/bin}"
TARGET="${DEST}/bucks"

# Record the OLD version (if any) for the update report, before we overwrite.
OLD_VER=""
if [ -x "$TARGET" ]; then
  OLD_VER="$("$TARGET" version 2>/dev/null | head -n1 || true)"
fi

mkdir -p "$DEST"
cp "$BIN_SRC" "$TARGET"
chmod +x "$TARGET"

NEW_VER="$("$TARGET" version 2>/dev/null | head -n1 || true)"
[ -n "$NEW_VER" ] || NEW_VER="(installed)"

echo
if [ -n "$OLD_VER" ]; then
  echo "Updated BUCKS:"
  echo "  was: ${OLD_VER}"
  echo "  now: ${NEW_VER}"
else
  echo "Installed BUCKS: ${NEW_VER}"
fi
echo "Binary: ${TARGET}"

# ---- PATH hint --------------------------------------------------------------
case ":${PATH}:" in
  *":${DEST}:"*) ON_PATH=1 ;;
  *)             ON_PATH=0 ;;
esac
if [ "$ON_PATH" -ne 1 ]; then
  echo
  echo "NOTE: ${DEST} is not on your PATH. Add this line to your shell profile"
  echo "      (~/.bashrc, ~/.zshrc, or ~/.profile), then open a new terminal:"
  echo
  echo "      export PATH=\"${DEST}:\$PATH\""
fi

# ---- next steps (do NOT auto-launch under a pipe) ---------------------------
echo
echo "Run:           bucks"
echo "Help:          bucks help"
echo "Update later:  re-run this command, or 'bucks update'"
