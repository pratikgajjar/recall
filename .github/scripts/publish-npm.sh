#!/usr/bin/env bash
# Publishes the per-platform recall-bin-* packages and @pratikgajjar/pi-recall
# from artifacts GoReleaser just put in dist/. Pattern: same as fff / esbuild
# / swc — main package lists per-platform binaries as optionalDependencies, so
# npm only fetches the one matching the host's os+arch.
#
# Requires: NODE_AUTH_TOKEN env (npm automation token, scoped to @pratikgajjar).
# Inputs:   GITHUB_REF_NAME (e.g. "v0.1.4") or VERSION env.
# Set DRY_RUN=1 to assemble packages locally without publishing.

set -euo pipefail

VERSION="${VERSION:-${GITHUB_REF_NAME#v}}"
if [[ -z "$VERSION" || "$VERSION" == v* ]]; then
  echo "error: VERSION not resolved (got '$VERSION')" >&2
  exit 1
fi
SCOPE="@pratikgajjar"
REPO="pratikgajjar/recall"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# GoReleaser arch name → npm arch name. (npm uses process.arch values: x64, arm64.)
declare -A GR_TO_NPM=(
  ["darwin_amd64"]="darwin-x64"
  ["darwin_arm64"]="darwin-arm64"
  ["linux_amd64"]="linux-x64"
  ["linux_arm64"]="linux-arm64"
)

publish_bin_package() {
  local gr_plat="$1" npm_plat="$2"
  local os="${npm_plat%-*}" arch="${npm_plat#*-}"
  local name="${SCOPE}/recall-bin-${npm_plat}"
  local dir="${TMP}/${npm_plat}"
  local tarball="dist/recall_${VERSION}_${gr_plat}.tar.gz"

  if [[ ! -f "$tarball" ]]; then
    echo "error: expected GoReleaser tarball not found: $tarball" >&2
    exit 1
  fi

  mkdir -p "$dir"
  tar -xzf "$tarball" -C "$dir" recall
  chmod +x "${dir}/recall"

  cat > "${dir}/package.json" <<EOF
{
  "name": "${name}",
  "version": "${VERSION}",
  "description": "Prebuilt recall binary for ${os} ${arch}",
  "os": ["${os}"],
  "cpu": ["${arch}"],
  "files": ["recall"],
  "license": "MIT",
  "publishConfig": { "access": "public" },
  "repository": {
    "type": "git",
    "url": "git+https://github.com/${REPO}.git"
  },
  "homepage": "https://github.com/${REPO}"
}
EOF

  if [[ "${DRY_RUN:-0}" == "1" ]]; then
    echo "[dry-run] would publish ${name}@${VERSION}"
  else
    echo "publishing ${name}@${VERSION}…"
    (cd "$dir" && npm publish --access public --provenance)
  fi
}

for gr_plat in "${!GR_TO_NPM[@]}"; do
  publish_bin_package "$gr_plat" "${GR_TO_NPM[$gr_plat]}"
done

# Sync the extension's version + optionalDependencies, then publish it.
EXT_DIR="packages/pi-recall"
node -e "
  const fs = require('fs');
  const path = '${EXT_DIR}/package.json';
  const p = JSON.parse(fs.readFileSync(path, 'utf8'));
  p.version = '${VERSION}';
  p.optionalDependencies = {};
  for (const plat of ['darwin-arm64','darwin-x64','linux-arm64','linux-x64']) {
    p.optionalDependencies['${SCOPE}/recall-bin-' + plat] = '${VERSION}';
  }
  fs.writeFileSync(path, JSON.stringify(p, null, 2) + '\n');
"

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  echo "[dry-run] would publish ${SCOPE}/pi-recall@${VERSION}"
else
  echo "publishing ${SCOPE}/pi-recall@${VERSION}…"
  (cd "$EXT_DIR" && npm publish --access public --provenance)
fi
