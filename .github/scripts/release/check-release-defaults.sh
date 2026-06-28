#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${VERSION:-}" ]]; then
  echo "::error::VERSION is required"
  exit 1
fi

failures=0

require_file() {
  local file="$1"
  if [[ ! -f "$file" ]]; then
    echo "::error::Required release-defaults file is missing: $file"
    failures=$((failures + 1))
    return 1
  fi
}

expect_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"

  require_file "$file" || return 0
  if ! grep -Fq "$needle" "$file"; then
    echo "::error file=$file::Expected $label to contain: $needle"
    failures=$((failures + 1))
  fi
}

expect_contains "scripts/install.py" "DEFAULT_TAG = \"${VERSION}\"" "installer DEFAULT_TAG"
expect_contains "README.md" "| \`--tag\` | \`${VERSION}\` |" "English README --tag default"
expect_contains "README.zh-CN.md" "| \`--tag\` | \`${VERSION}\` |" "Chinese README --tag default"
expect_contains "website/src/docs/en/install.mdx" "| \`--tag\` | \`${VERSION}\` |" "English website install --tag default"
expect_contains "website/src/docs/zh-CN/install.mdx" "| \`--tag\` | \`${VERSION}\` |" "Chinese website install --tag default"
expect_contains "website/src/features/docs/TagSelector.tsx" "defaultTag = '${VERSION}'" "website tag selector defaultTag"
expect_contains "web/public/static/worker-startup.sh" "DEFAULT_TAG=\"${VERSION}\"" "worker startup DEFAULT_TAG"
expect_contains "web/public/static/worker-startup.sh" "Defaults to ${VERSION}." "worker startup usage default"
expect_contains "web/vite.config.ts" ": '${VERSION}'" "web workerStartupDefaultTag fallback"
expect_contains "web/src/composables/useWorkerStartupTool.ts" "? '${VERSION}'" "temporary probe installer fallback"
expect_contains "web/src/components/worker-tool/WorkerSysConfigForm.vue" "Leave as ${VERSION} for the current default." "temporary probe help text"
expect_contains "web/src/components/worker-tool/WorkerSysConfigForm.vue" "placeholder=\"${VERSION}\"" "temporary probe placeholder"

if (( failures > 0 )); then
  echo "::error::Release defaults are not synchronized with $VERSION. See README/release-defaults.md."
  exit 1
fi

echo "Release defaults are synchronized with $VERSION."
