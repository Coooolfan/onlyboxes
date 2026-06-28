#!/usr/bin/env bash
set -euo pipefail

SEMVER_REGEX='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$'

if [[ "$INPUT_VERSION" == v* ]]; then
  echo "::error::Onlyboxes release tags do not use a v prefix. Use 0.7.2 instead of v0.7.2."
  exit 1
fi

if [[ ! "$INPUT_VERSION" =~ $SEMVER_REGEX ]]; then
  echo "::error::Version must look like 0.7.2 or 0.7.2-rc.1. Prerelease labels are limited to alpha, beta, and rc."
  exit 1
fi

case "$INPUT_LATEST" in
  true | false) ;;
  *)
    echo "::error::latest must be true or false, got $INPUT_LATEST"
    exit 1
    ;;
esac

git fetch --force --tags origin
TARGET_SHA="$(git rev-parse --verify HEAD)"
VERSION_SAFE="$(printf '%s' "$INPUT_VERSION" | sed -E 's/[^A-Za-z0-9._-]+/-/g')"

if [[ "$INPUT_VERSION" == *-* ]]; then
  PRERELEASE=true
else
  PRERELEASE=false
fi

if [[ "$PRERELEASE" == "true" && "$INPUT_LATEST" == "true" ]]; then
  echo "::error::Prerelease versions cannot be published as latest."
  exit 1
fi

NOTES_FILE="docs/release_notes/${INPUT_VERSION}.md"
if [[ ! -f "$NOTES_FILE" ]]; then
  echo "::error::Release notes file is required: $NOTES_FILE"
  exit 1
fi

EXISTING_TAG_SHA="$(git rev-parse --verify --quiet "refs/tags/$INPUT_VERSION^{}" || true)"
if [[ -n "$EXISTING_TAG_SHA" ]]; then
  if [[ "$EXISTING_TAG_SHA" != "$TARGET_SHA" ]]; then
    echo "::error::Tag $INPUT_VERSION already exists at $EXISTING_TAG_SHA, not target $TARGET_SHA"
    exit 1
  fi

  if RELEASE_JSON="$(gh release view "$INPUT_VERSION" --json isDraft,isPrerelease 2>/dev/null)"; then
    RELEASE_DRAFT="$(jq -r .isDraft <<< "$RELEASE_JSON")"
    RELEASE_PRERELEASE="$(jq -r .isPrerelease <<< "$RELEASE_JSON")"
    if [[ "$RELEASE_DRAFT" != "true" ]]; then
      echo "::error::Release $INPUT_VERSION is already published and cannot be rerun."
      exit 1
    fi
    if [[ "$RELEASE_PRERELEASE" != "$PRERELEASE" ]]; then
      echo "::error::Draft release prerelease=$RELEASE_PRERELEASE does not match this run prerelease=$PRERELEASE"
      exit 1
    fi
    echo "::notice::Found reusable draft release $INPUT_VERSION"
  else
    echo "::notice::Found reusable tag $INPUT_VERSION without a release"
  fi
fi

if [[ "$INPUT_LATEST" == "true" ]]; then
  git fetch --force origin main
  MAIN_SHA="$(git rev-parse --verify origin/main)"
  if [[ "$TARGET_SHA" != "$MAIN_SHA" ]]; then
    echo "::error::latest can only be published from origin/main HEAD. workflow_ref=$GITHUB_REF_NAME target_sha=$TARGET_SHA origin/main=$MAIN_SHA"
    exit 1
  fi
fi

{
  echo "version=$INPUT_VERSION"
  echo "version_safe=$VERSION_SAFE"
  echo "target_sha=$TARGET_SHA"
  echo "prerelease=$PRERELEASE"
  echo "publish_latest=$INPUT_LATEST"
} >> "$GITHUB_OUTPUT"
