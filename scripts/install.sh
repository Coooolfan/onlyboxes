#!/usr/bin/env bash
set -euo pipefail

if ! command -v python3 >/dev/null 2>&1; then
  echo "Error: python3 is required but not found." >&2
  echo "Install Python 3 and try again." >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_PY="$SCRIPT_DIR/install.py"

if [[ -f "$INSTALL_PY" ]]; then
  exec python3 "$INSTALL_PY" "$@"
fi

# Piped execution (e.g. curl | bash): download install.py from GitHub.
ORIG_ARGS=("$@")
TAG=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)  TAG="$2"; break ;;
    --tag=*) TAG="${1#--tag=}"; break ;;
    *) shift ;;
  esac
done

if [[ -z "$TAG" ]]; then
  echo "Error: --tag is required." >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

INSTALL_PY_URL="https://raw.githubusercontent.com/Coooolfan/onlyboxes/${TAG}/scripts/install.py"
echo "Downloading install.py ..."

if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$INSTALL_PY_URL" -o "$TMPDIR/install.py"
elif command -v wget >/dev/null 2>&1; then
  wget -q "$INSTALL_PY_URL" -O "$TMPDIR/install.py"
else
  echo "Error: curl or wget is required." >&2
  exit 1
fi

exec python3 "$TMPDIR/install.py" "${ORIG_ARGS[@]}" < /dev/tty
