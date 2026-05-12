#!/usr/bin/env bash
set -euo pipefail

DEFAULT_TAG="0.4.0"
REPO="onlyboxes/onlyboxes"

worker_id=""
worker_secret=""
grpc_target=""
tag="${DEFAULT_TAG}"

usage() {
  cat >&2 <<'EOF'
Usage: worker-startup.sh --worker-id ID --worker-secret SECRET --grpc-target HOST:PORT [--tag TAG]

Accepted options:
  --worker-id       Worker ID issued by the console.
  --worker-secret   One-time worker secret issued by the console.
  --grpc-target     Console gRPC target in host:port form.
  --tag             GitHub release tag to download. Defaults to 0.4.0.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --worker-id)
      [[ $# -ge 2 ]] || { echo "missing value for --worker-id" >&2; usage; exit 2; }
      worker_id="$2"
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

if [[ -z "${worker_id}" || -z "${worker_secret}" || -z "${grpc_target}" ]]; then
  echo "--worker-id, --worker-secret, and --grpc-target are required" >&2
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

cleanup() {
  rm -f "${runtime_dir}/${asset_zip}"
}
trap cleanup EXIT

echo "Downloading ${download_url}" >&2
if command -v curl >/dev/null 2>&1; then
  curl -fL --retry 3 --connect-timeout 15 -o "${runtime_dir}/${asset_zip}" "${download_url}"
elif command -v wget >/dev/null 2>&1; then
  wget -O "${runtime_dir}/${asset_zip}" "${download_url}"
else
  echo "curl or wget is required" >&2
  exit 1
fi

if command -v unzip >/dev/null 2>&1; then
  unzip -q -o "${runtime_dir}/${asset_zip}" -d "${runtime_dir}"
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

export WORKER_ID="${worker_id}"
export WORKER_SECRET="${worker_secret}"
export WORKER_CONSOLE_GRPC_TARGET="${grpc_target}"
export WORKER_NODE_NAME="Temporary Probe"
export WORKER_CONSOLE_INSECURE="true"
export WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE="allow_all"
export WORKER_READ_IMAGE_ALLOWED_PATHS='["/"]'

exec "${worker_bin}"
