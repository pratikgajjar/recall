#!/usr/bin/env bash
# Bundles the per-platform recall binaries from GoReleaser's dist/ into the
# pi-recall extension's bin/ directory and publishes the single
# @pratikgajjar/pi-recall package. One package, all platforms — the extension
# picks the matching binary at runtime (see resolveBundledBin in src/index.ts).
#
# Auth: OIDC trusted publishing (configured on npmjs.com for @pratikgajjar/
# pi-recall); no token needed. Provenance via the workflow's id-token.
# Inputs: GITHUB_REF_NAME (e.g. "v0.2.0") or VERSION env.
# Set DRY_RUN=1 to assemble the package locally without publishing.

set -euo pipefail

VERSION="${VERSION:-${GITHUB_REF_NAME#v}}"
if [[ -z "$VERSION" || "$VERSION" == v* ]]; then
  echo "error: VERSION not resolved (got '$VERSION')" >&2
  exit 1
fi
REPO="pratikgajjar/recall"
EXT_DIR="packages/pi-recall"
BIN_DIR="${EXT_DIR}/bin"

# GoReleaser platform → bundled binary name (process.platform-process.arch).
declare -A GR_TO_NODE=(
  ["darwin_amd64"]="darwin-x64"
  ["darwin_arm64"]="darwin-arm64"
  ["linux_amd64"]="linux-x64"
  ["linux_arm64"]="linux-arm64"
)

rm -rf "$BIN_DIR"
mkdir -p "$BIN_DIR"
for gr_plat in "${!GR_TO_NODE[@]}"; do
  node_plat="${GR_TO_NODE[$gr_plat]}"
  tarball="dist/recall_${VERSION}_${gr_plat}.tar.gz"
  if [[ ! -f "$tarball" ]]; then
    echo "error: expected GoReleaser tarball not found: $tarball" >&2
    exit 1
  fi
  tar -xzf "$tarball" -O recall > "${BIN_DIR}/recall-${node_plat}"
  chmod +x "${BIN_DIR}/recall-${node_plat}"
  echo "bundled recall-${node_plat}"
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
  (cd "$EXT_DIR" && npm publish --access public --provenance)
fi
