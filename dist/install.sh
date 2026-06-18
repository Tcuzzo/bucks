#!/usr/bin/env bash
# install.sh — BUCKS guided first-run unpack (Linux / macOS).
# This ships INSIDE each release zip next to the `bucks` binary. It is the friendly
# entry point: it puts the binary somewhere on your PATH (or runs it in place) and
# launches the guided setup wizard. It is plain and safe — it does not touch your
# system beyond an optional copy into a user-local bin dir you approve.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$HERE/bucks"

if [[ ! -x "$BIN" ]]; then
  echo "BUCKS: cannot find the bucks binary next to this installer ($BIN)." >&2
  echo "Make sure you unzipped the whole BUCKS_<os>_<arch>.zip and ran install.sh from inside it." >&2
  exit 1
fi

cat <<'BANNER'
BUCKS — a trading agent, a predator not an assistant.

Welcome. BUCKS starts in PAPER mode (simulated money) — going live is a deliberate
choice you make later. Let's get you set up.
BANNER

# Offer to install onto PATH (~/.local/bin), else run in place. Never sudo silently.
DEST="${HOME}/.local/bin"
read -r -p "Install 'bucks' into ${DEST} so you can run it from anywhere? [Y/n] " ans || ans="y"
case "${ans:-y}" in
  [Nn]*)
    echo "OK — running BUCKS from this folder. You can copy '$BIN' onto your PATH later."
    ;;
  *)
    mkdir -p "$DEST"
    cp "$BIN" "$DEST/bucks"
    chmod +x "$DEST/bucks"
    BIN="$DEST/bucks"
    echo "Installed to $BIN"
    case ":$PATH:" in
      *":$DEST:"*) : ;;
      *) echo "NOTE: $DEST is not on your PATH. Add it to your shell profile to run 'bucks' from anywhere." ;;
    esac
    ;;
esac

echo
echo "Launching the BUCKS setup wizard..."
exec "$BIN"
