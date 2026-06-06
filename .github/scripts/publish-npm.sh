#!/usr/bin/env bash
# Cross-builds the recall binary for every supported platform into the
# pi-recall extension's bin/ directory, then publishes the single
# @pratikgajjar/pi-recall package (binaries bundled, picked at runtime by
# resolveBundledBin in src/index.ts). Self-contained: builds from this Go
# module, no dependency on GoReleaser's dist/.
#
# Auth: npm Trusted Publishers (OIDC). Must run from the workflow npm trusts
# (.github/workflows/publish-npm.yml); no NPM_TOKEN.
# Inputs: GITHUB_REF_NAME (e.g. "v0.2.2") or VERSION env.
# Set DRY_RUN=1 to build + assemble locally without publishing.

set -euo pipefail

VERSION="${VERSION:-${GITHUB_REF_NAME#v}}"
if [[ -z "$VERSION" || "$VERSION" == v* ]]; then
  echo "error: VERSION not resolved (got '$VERSION')" >&2
  exit 1
fi
EXT_DIR="packages/pi-recall"
BIN_DIR="${EXT_DIR}/bin"

# bundled binary name (process.platform-process.arch) -> "GOOS GOARCH"
declare -A TARGETS=(
  ["darwin-x64"]="darwin amd64"
  ["darwin-arm64"]="darwin arm64"
  ["linux-x64"]="linux amd64"
  ["linux-arm64"]="linux arm64"
)

rm -rf "$BIN_DIR"
mkdir -p "$BIN_DIR"
for node_plat in "${!TARGETS[@]}"; do
  read -r goos goarch <<< "${TARGETS[$node_plat]}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o "${BIN_DIR}/recall-${node_plat}" .
  chmod +x "${BIN_DIR}/recall-${node_plat}"
  echo "built recall-${node_plat}"
done

# Pin the published version (the in-repo default is a dev placeholder).
node -e "
  const fs = require('fs');
  const path = '${EXT_DIR}/package.json';
  const p = JSON.parse(fs.readFileSync(path, 'utf8'));
  p.version = '${VERSION}';
  fs.writeFileSync(path, JSON.stringify(p, null, 2) + '\n');
"

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  echo "[dry-run] assembled ${EXT_DIR} with $(ls "$BIN_DIR" | wc -l | tr -d ' ') binaries; would publish @pratikgajjar/pi-recall@${VERSION}"
else
  echo "publishing @pratikgajjar/pi-recall@${VERSION}…"
  (cd "$EXT_DIR" && npm publish --provenance --access public)
fi
