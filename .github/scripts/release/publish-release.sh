#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${VERSION:-}" || -z "${PUBLISH_LATEST:-}" ]]; then
  echo "::error::VERSION and PUBLISH_LATEST are required"
  exit 1
fi

case "$PUBLISH_LATEST" in
  true | false) ;;
  *)
    echo "::error::PUBLISH_LATEST must be true or false, got $PUBLISH_LATEST"
    exit 1
    ;;
esac

RELEASE_ID="$(gh release view "$VERSION" --json databaseId --jq .databaseId)"
gh api --method PATCH "repos/{owner}/{repo}/releases/$RELEASE_ID" \
  -F draft=false \
  -f "make_latest=$PUBLISH_LATEST" \
  --silent
