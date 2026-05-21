#!/usr/bin/env bash
set -euo pipefail

DEFAULT_TAG="0.6.1"
REPO="Coooolfan/onlyboxes"

node_id=""
worker_secret=""
grpc_target=""
console_insecure=""
tag="${DEFAULT_TAG}"

usage() {
  cat >&2 <<'EOF'
Usage: worker-startup.sh --node-id ID --worker-secret SECRET --grpc-target HOST:PORT [--console-insecure true|false] [--tag TAG]

Accepted options:
  --node-id           Node ID issued by the console.
  --worker-secret     One-time worker secret issued by the console.
  --grpc-target       Console gRPC target in host:port form.
  --console-insecure  Set WORKER_CONSOLE_INSECURE. Pass "true" for plaintext gRPC; omit or "false" for TLS.
  --tag               GitHub release tag to download. Defaults to 0.6.1.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --node-id)
      [[ $# -ge 2 ]] || { echo "missing value for --node-id" >&2; usage; exit 2; }
      node_id="$2"
      shift 2
      ;;
    --worker-secret)
      [[ $# -ge 2 ]] || { echo "missing value for --worker-secret" >&2; usage; exit 2; }
      worker_secret="$2"
      shift 2
      ;;
    --grpc-target)
      [[ $# -ge 2 ]] || { echo "missing value for --grpc-target" >&2; usage; exit 2; }
      grpc_target="$2"
      shift 2
      ;;
    --console-insecure)
      [[ $# -ge 2 ]] || { echo "missing value for --console-insecure" >&2; usage; exit 2; }
      case "$2" in
        true|false) console_insecure="$2" ;;
        *) echo "--console-insecure expects 'true' or 'false', got: $2" >&2; usage; exit 2 ;;
      esac
      shift 2
      ;;
    --tag)
      [[ $# -ge 2 ]] || { echo "missing value for --tag" >&2; usage; exit 2; }
      tag="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ -z "${node_id}" || -z "${worker_secret}" || -z "${grpc_target}" ]]; then
  echo "--node-id, --worker-secret, and --grpc-target are required" >&2
  usage
  exit 2
fi

case "$(uname -s)" in
  Linux) os_name="linux" ;;
  Darwin) os_name="macos" ;;
  *)
    echo "unsupported operating system: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch_name="amd64" ;;
  arm64|aarch64) arch_name="arm64" ;;
  *)
    echo "unsupported CPU architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_safe="$(printf '%s' "${tag}" | sed -E 's/[^A-Za-z0-9._-]+/-/g')"
if [[ -z "${version_safe}" ]]; then
  echo "sanitized release tag is empty" >&2
  exit 1
fi

asset_name="onlyboxes-worker-sys_${version_safe}_${os_name}_${arch_name}"
asset_zip="${asset_name}.zip"
download_url="https://github.com/${REPO}/releases/download/${tag}/${asset_zip}"
runtime_dir="$(mktemp -d "${TMPDIR:-/tmp}/onlyboxes-worker-sys.XXXXXX")"
local_asset_zip="./${asset_zip}"
runtime_asset_zip="${runtime_dir}/${asset_zip}"

cleanup() {
  rm -rf "${runtime_dir}"
}
trap cleanup EXIT INT TERM

if [[ -f "${local_asset_zip}" ]]; then
  echo "Using local ${asset_zip}" >&2
  cp "${local_asset_zip}" "${runtime_asset_zip}"
else
  echo "Downloading ${download_url}" >&2
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --connect-timeout 15 -o "${runtime_asset_zip}" "${download_url}"
  elif command -v wget >/dev/null 2>&1; then
    wget -O "${runtime_asset_zip}" "${download_url}"
  else
    echo "curl or wget is required" >&2
    exit 1
  fi
fi

if command -v unzip >/dev/null 2>&1; then
  unzip -q -o "${runtime_asset_zip}" -d "${runtime_dir}"
else
  echo "unzip is required" >&2
  exit 1
fi

worker_bin="${runtime_dir}/${asset_name}"
if [[ ! -f "${worker_bin}" ]]; then
  worker_bin="$(find "${runtime_dir}" -maxdepth 1 -type f -name 'onlyboxes-worker-sys*' | head -n 1 || true)"
fi
if [[ -z "${worker_bin}" || ! -f "${worker_bin}" ]]; then
  echo "worker-sys binary not found in ${asset_zip}" >&2
  exit 1
fi
chmod +x "${worker_bin}"

export WORKER_ID="${node_id}"
export WORKER_SECRET="${worker_secret}"
export WORKER_CONSOLE_GRPC_TARGET="${grpc_target}"
export WORKER_NODE_NAME="Temporary Probe"
if [[ "${console_insecure}" == "true" ]]; then
  export WORKER_CONSOLE_INSECURE="true"
fi
export WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE="allow_all"
export WORKER_READ_IMAGE_ALLOWED_PATHS='["/"]'

"${worker_bin}"
