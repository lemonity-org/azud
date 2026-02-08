#!/bin/sh
# Azud installer — downloads a pre-built binary from GitHub Releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lemonity-org/azud/main/scripts/install.sh | sh
#
# Environment variables:
#   AZUD_VERSION      Version to install (default: latest)
#   AZUD_INSTALL_DIR  Installation directory (default: $HOME/.azud/bin)

set -eu

REPO="lemonity-org/azud"
INSTALL_DIR="${AZUD_INSTALL_DIR:-$HOME/.azud/bin}"
VERSION="${AZUD_VERSION:-latest}"

main() {
  detect_platform
  resolve_version
  download_and_verify
  install_binary
  print_success
}

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      fatal "Unsupported OS: $OS" ;;
  esac

  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             fatal "Unsupported architecture: $ARCH" ;;
  esac

  log "Detected platform: ${OS}/${ARCH}"
}

resolve_version() {
  if [ "$VERSION" = "latest" ]; then
    log "Resolving latest version..."
    # Try stable release first, fall back to any release (including pre-releases)
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
      | grep '"tag_name"' \
      | head -1 \
      | sed 's/.*"tag_name": *"//;s/".*//')
    if [ -z "$VERSION" ]; then
      log "No stable release found, checking pre-releases..."
      VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases" \
        | grep '"tag_name"' \
        | head -1 \
        | sed 's/.*"tag_name": *"//;s/".*//')
    fi
    if [ -z "$VERSION" ]; then
      fatal "Could not determine latest version. Set AZUD_VERSION explicitly."
    fi
  fi
  log "Version: ${VERSION}"
}

download_and_verify() {
  BINARY_NAME="azud-${OS}-${ARCH}"
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

  TMPDIR=$(mktemp -d)
  trap 'rm -rf "$TMPDIR"' EXIT

  log "Downloading ${BINARY_NAME}..."
  curl -fsSL -o "${TMPDIR}/${BINARY_NAME}" "${BASE_URL}/${BINARY_NAME}"

  log "Downloading checksums..."
  curl -fsSL -o "${TMPDIR}/checksums.txt" "${BASE_URL}/checksums.txt"

  log "Verifying checksum..."
  EXPECTED=$(grep "${BINARY_NAME}$" "${TMPDIR}/checksums.txt" | awk '{print $1}')
  if [ -z "$EXPECTED" ]; then
    fatal "Checksum not found for ${BINARY_NAME}"
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL=$(sha256sum "${TMPDIR}/${BINARY_NAME}" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "${TMPDIR}/${BINARY_NAME}" | awk '{print $1}')
  else
    fatal "No sha256sum or shasum found — cannot verify checksum"
  fi

  if [ "$EXPECTED" != "$ACTUAL" ]; then
    fatal "Checksum mismatch!\n  Expected: ${EXPECTED}\n  Actual:   ${ACTUAL}"
  fi
  log "Checksum verified."
}

install_binary() {
  mkdir -p "$INSTALL_DIR"
  mv "${TMPDIR}/${BINARY_NAME}" "${INSTALL_DIR}/azud"
  chmod +x "${INSTALL_DIR}/azud"
  log "Installed azud to ${INSTALL_DIR}/azud"
}

print_success() {
  INSTALLED_VERSION=$("${INSTALL_DIR}/azud" version 2>/dev/null || echo "${VERSION}")
  cat <<EOF

  azud ${INSTALLED_VERSION} installed successfully!

  Add azud to your PATH (if not already):
    export PATH="\$HOME/.azud/bin:\$PATH"

EOF
}

log() {
  printf '  → %s\n' "$1"
}

fatal() {
  printf 'Error: %s\n' "$1" >&2
  exit 1
}

main
