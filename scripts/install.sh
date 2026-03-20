#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "Error: python3 is required but not found." >&2
  echo "Install Python 3 and try again." >&2
  exit 1
fi

exec python3 "$SCRIPT_DIR/install.py" "$@"
