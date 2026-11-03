#!/bin/sh
# Azud installer - downloads a pre-built binary from GitHub Releases.
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

# Verifies a release on a machine without the GitHub CLI. Keep in step with
# .github/workflows/release.yml, which signs with the same version.
COSIGN_VERSION="v3.1.4"

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

  AZUD_INSTALL_TMP=$(mktemp -d)
  trap 'rm -rf "$AZUD_INSTALL_TMP"' EXIT

  log "Downloading ${BINARY_NAME}..."
  curl -fsSL -o "${AZUD_INSTALL_TMP}/${BINARY_NAME}" "${BASE_URL}/${BINARY_NAME}"

  log "Downloading checksums..."
  curl -fsSL -o "${AZUD_INSTALL_TMP}/checksums.txt" "${BASE_URL}/checksums.txt"

  log "Verifying checksum..."
  EXPECTED=$(grep "${BINARY_NAME}$" "${AZUD_INSTALL_TMP}/checksums.txt" | awk '{print $1}')
  if [ -z "$EXPECTED" ]; then
    fatal "Checksum not found for ${BINARY_NAME}"
  fi

  ACTUAL=$(sha256_of "${AZUD_INSTALL_TMP}/${BINARY_NAME}")
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    fatal "Checksum mismatch: expected ${EXPECTED}; got ${ACTUAL}"
  fi
  log "Checksum verified."

  log "Verifying GitHub/Sigstore build provenance..."
  if command -v gh >/dev/null 2>&1; then
    if ! gh attestation verify "${AZUD_INSTALL_TMP}/${BINARY_NAME}" --repo "$REPO" >/dev/null; then
      fatal "Release provenance verification failed for ${BINARY_NAME}"
    fi
  else
    verify_signed_checksums
  fi
  log "Build provenance verified."
}

# The binary already matches checksums.txt, so a valid signature over that file
# from this repository's release workflow vouches for the binary too.
verify_signed_checksums() {
  BUNDLE="${AZUD_INSTALL_TMP}/checksums.txt.sigstore.json"
  if ! curl -fsSL -o "$BUNDLE" "${BASE_URL}/checksums.txt.sigstore.json" 2>/dev/null; then
    fatal "${VERSION} has no signed checksums, so verifying it needs the GitHub CLI (gh): https://cli.github.com/"
  fi

  if ! COSIGN=$(command -v cosign); then
    fetch_cosign
  fi

  if ! "$COSIGN" verify-blob "${AZUD_INSTALL_TMP}/checksums.txt" \
    --bundle "$BUNDLE" \
    --certificate-identity "https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${VERSION}" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    >/dev/null 2>"${AZUD_INSTALL_TMP}/cosign.log"; then
    cat "${AZUD_INSTALL_TMP}/cosign.log" >&2
    fatal "Signature verification failed for checksums.txt (this needs cosign 3 or later)"
  fi
}

fetch_cosign() {
  case "${OS}-${ARCH}" in
    linux-amd64)  COSIGN_SHA256="424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa" ;;
    linux-arm64)  COSIGN_SHA256="ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2" ;;
    darwin-amd64) COSIGN_SHA256="9d1e4abe2b6be279ed0d2ff706fde1ccd738747c834e3e6d44039c238867334f" ;;
    darwin-arm64) COSIGN_SHA256="9f9b8cd557cf2e5b3df3373a00baa005a86b02391e3e06dd0ce37b3a1533d3a4" ;;
    *)            fatal "No cosign pinned for ${OS}-${ARCH}" ;;
  esac

  log "Neither gh nor cosign found, downloading cosign ${COSIGN_VERSION} to verify this release..."
  COSIGN="${AZUD_INSTALL_TMP}/cosign"
  curl -fsSL -o "$COSIGN" "https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/cosign-${OS}-${ARCH}"
  if [ "$(sha256_of "$COSIGN")" != "$COSIGN_SHA256" ]; then
    fatal "Checksum mismatch for the downloaded cosign"
  fi
  chmod +x "$COSIGN"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fatal "No sha256sum or shasum found - cannot verify checksum"
  fi
}

install_binary() {
  mkdir -p "$INSTALL_DIR"
  mv "${AZUD_INSTALL_TMP}/${BINARY_NAME}" "${INSTALL_DIR}/azud"
  chmod +x "${INSTALL_DIR}/azud"
  log "Installed azud to ${INSTALL_DIR}/azud"
}

print_success() {
  if INSTALLED_VERSION=$("${INSTALL_DIR}/azud" version --short 2>/dev/null); then
    :
  else
    # Compatibility with binaries released before version --short existed.
    INSTALLED_VERSION=$("${INSTALL_DIR}/azud" version 2>/dev/null | awk 'NR == 1 { print $2 }')
  fi
  if [ -z "$INSTALLED_VERSION" ]; then
    INSTALLED_VERSION="$VERSION"
  fi
  cat <<EOF

AZUD / INSTALL
--------------------------------------------------------
  OK     azud ${INSTALLED_VERSION}
  PATH   ${INSTALL_DIR}/azud
  NEXT   export PATH="\$HOME/.azud/bin:\$PATH"
EOF
}

log() {
  printf '  INFO   %s\n' "$1"
}

fatal() {
  printf '  ERROR  %s\n' "$1" >&2
  exit 1
}

main
