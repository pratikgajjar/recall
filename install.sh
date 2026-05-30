#!/usr/bin/env sh
# recall installer — downloads the latest release binary for your platform.
#
#   curl -fsSL https://raw.githubusercontent.com/pratikgajjar/recall/main/install.sh | sh
#
# Override the install dir with RECALL_INSTALL_DIR (default: ~/.local/bin).
set -eu

REPO="pratikgajjar/recall"
BIN="recall"
INSTALL_DIR="${RECALL_INSTALL_DIR:-$HOME/.local/bin}"

info()  { printf '\033[1;34m%s\033[0m\n' "$*"; }
ok()    { printf '\033[1;32m%s\033[0m\n' "$*"; }
warn()  { printf '\033[1;33m%s\033[0m\n' "$*"; }
err()   { printf '\033[1;31merror: %s\033[0m\n' "$*" >&2; exit 1; }

detect() {
  os=$(uname -s)
  arch=$(uname -m)
  case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) err "unsupported OS: $os (try: go install github.com/$REPO@latest)" ;;
  esac
  case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) err "unsupported architecture: $arch" ;;
  esac
  echo "${os}_${arch}"
}

latest_tag() {
  # Resolve the latest release tag without needing jq.
  curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/'
}

main() {
  command -v curl >/dev/null 2>&1 || err "curl is required"
  command -v tar  >/dev/null 2>&1 || err "tar is required"

  platform=$(detect)
  tag=$(latest_tag)
  [ -n "$tag" ] || err "could not determine latest release"
  version=${tag#v}

  asset="${BIN}_${version}_${platform}.tar.gz"
  url="https://github.com/$REPO/releases/download/$tag/$asset"

  info "downloading $asset ($tag)"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL "$url" -o "$tmp/$asset" || err "download failed: $url"
  tar -xzf "$tmp/$asset" -C "$tmp" "$BIN" || err "extract failed"

  mkdir -p "$INSTALL_DIR"
  mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
  chmod +x "$INSTALL_DIR/$BIN"
  ok "installed $BIN $tag -> $INSTALL_DIR/$BIN"

  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) warn "note: $INSTALL_DIR is not on your PATH — add it:"
       printf '  export PATH="%s:$PATH"\n' "$INSTALL_DIR" ;;
  esac

  if "$INSTALL_DIR/$BIN" version >/dev/null 2>&1; then
    info "next: run '$BIN index' once, then '$BIN doctor'"
  fi
}

main "$@"
