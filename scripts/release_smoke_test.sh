#!/bin/sh

set -eu

TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT

MOCK_BIN="$TEST_TMP/bin"
INSTALL_DIR="$TEST_TMP/install"
MOCK_BINARY="$TEST_TMP/azud-release"
MOCK_GH_ARGS="$TEST_TMP/gh-args"
MOCK_REGISTRY_ARGS="$TEST_TMP/registry-args"
MOCK_REGISTRY_STDIN="$TEST_TMP/registry-stdin"
mkdir -p "$MOCK_BIN"

cat > "$MOCK_BINARY" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  if [ "${2:-}" = "--short" ]; then
    if [ "${MOCK_LEGACY_VERSION:-}" = "1" ]; then
      exit 1
    fi
    echo "v1.2.3"
  else
    echo "Azud v1.2.3"
    echo "  COMMIT  abc1234"
    echo "  BUILT   2026-07-19T00:00:00Z"
    echo "  GO      go1.25.0"
    echo "  TARGET  linux/amd64"
  fi
fi
EOF
chmod +x "$MOCK_BINARY"

cat > "$MOCK_BIN/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
  -s) echo Linux ;;
  -m) echo x86_64 ;;
  *) exit 1 ;;
esac
EOF

cat > "$MOCK_BIN/curl" <<'EOF'
#!/bin/sh
out=""
url=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    out=$1
  else
    url=$1
  fi
  shift
done
case "$url" in
  */azud-linux-amd64)
    cp "$MOCK_BINARY" "$out"
    ;;
  */checksums.txt)
    digest=$(sha256sum "$MOCK_BINARY" | awk '{print $1}')
    printf '%s  azud-linux-amd64\n' "$digest" > "$out"
    ;;
  */checksums.txt.sigstore.json)
    [ -z "${MOCK_NO_BUNDLE:-}" ] || exit 22
    printf '{}\n' > "$out"
    ;;
  */cosign-linux-amd64)
    cp "$MOCK_COSIGN" "$out"
    ;;
  *)
    echo "unexpected curl URL: $url" >&2
    exit 1
    ;;
esac
EOF

cat > "$MOCK_BIN/gh" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" > "$MOCK_GH_ARGS"
[ "$1" = "attestation" ]
[ "$2" = "verify" ]
[ "$4" = "--repo" ]
[ "$5" = "lemonity-org/azud" ]
EOF

chmod +x "$MOCK_BIN/uname" "$MOCK_BIN/curl" "$MOCK_BIN/gh"

output=$(
  PATH="$MOCK_BIN:$PATH" \
  AZUD_VERSION=v1.2.3 \
  AZUD_INSTALL_DIR="$INSTALL_DIR" \
  MOCK_BINARY="$MOCK_BINARY" \
  MOCK_GH_ARGS="$MOCK_GH_ARGS" \
  sh scripts/install.sh
)

printf '%s\n' "$output" | grep -F "  OK     azud v1.2.3" >/dev/null
if printf '%s\n' "$output" | grep -F "COMMIT" >/dev/null; then
  echo "installer leaked multiline version output into its success banner" >&2
  exit 1
fi
if printf '%s\n' "$output" | LC_ALL=C grep "$(printf '\033')" >/dev/null; then
  echo "installer emitted ANSI escapes in captured output" >&2
  exit 1
fi
[ -x "$INSTALL_DIR/azud" ]
grep -F "attestation verify" "$MOCK_GH_ARGS" >/dev/null

# A server without the GitHub CLI verifies the signed checksums with cosign. These
# routes need a PATH that truly has no gh or cosign, whatever the machine has.
NO_GH_BIN="$TEST_TMP/no-gh-bin"
MOCK_COSIGN="$TEST_TMP/mock-cosign"
MOCK_COSIGN_ARGS="$TEST_TMP/cosign-args"
REAL_SHA256SUM=$(command -v sha256sum)
mkdir -p "$NO_GH_BIN"
for tool in awk cat chmod cp grep head mkdir mktemp mv rm sed sh tr; do
  ln -s "$(command -v "$tool")" "$NO_GH_BIN/$tool"
done
cp "$MOCK_BIN/curl" "$MOCK_BIN/uname" "$NO_GH_BIN/"
cat > "$MOCK_COSIGN" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" > "$MOCK_COSIGN_ARGS"
if [ -n "${MOCK_COSIGN_REJECTS:-}" ]; then
  echo "none of the expected identities matched" >&2
  exit 1
fi
EOF
chmod +x "$MOCK_COSIGN"

# The mock cosign cannot match the real pin, so sha256sum reports the pin for it
# unless a test asks for a mismatch.
pinned_digest=$(awk '/linux-amd64\)/ { sub(/.*COSIGN_SHA256="/, ""); sub(/".*/, ""); print }' scripts/install.sh)
cat > "$NO_GH_BIN/sha256sum" <<EOF
#!/bin/sh
case "\$1" in
  */cosign) echo "\${MOCK_COSIGN_DIGEST:-$pinned_digest}  \$1" ;;
  *) "$REAL_SHA256SUM" "\$@" ;;
esac
EOF
chmod +x "$NO_GH_BIN/sha256sum"

install_without_gh() {
  rm -f "$MOCK_COSIGN_ARGS"
  rm -rf "$INSTALL_DIR"
  PATH="$1" \
  AZUD_VERSION=v1.2.3 \
  AZUD_INSTALL_DIR="$INSTALL_DIR" \
  MOCK_BINARY="$MOCK_BINARY" \
  MOCK_COSIGN="$MOCK_COSIGN" \
  MOCK_COSIGN_ARGS="$MOCK_COSIGN_ARGS" \
  sh scripts/install.sh 2>&1
}

# No gh and no cosign: downloads the pinned cosign.
output=$(install_without_gh "$NO_GH_BIN")
printf '%s\n' "$output" | grep -F "downloading cosign" >/dev/null
[ -x "$INSTALL_DIR/azud" ]
grep -F "verify-blob" "$MOCK_COSIGN_ARGS" >/dev/null
grep -F "lemonity-org/azud/.github/workflows/release.yml@refs/tags/v1.2.3" "$MOCK_COSIGN_ARGS" >/dev/null
grep -F "https://token.actions.githubusercontent.com" "$MOCK_COSIGN_ARGS" >/dev/null

# A cosign that does not match the pin is never run.
if output=$(MOCK_COSIGN_DIGEST=0000 install_without_gh "$NO_GH_BIN"); then
  echo "installer accepted a cosign that does not match its pin" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -F "Checksum mismatch for the downloaded cosign" >/dev/null
[ ! -e "$MOCK_COSIGN_ARGS" ]
[ ! -e "$INSTALL_DIR/azud" ]

# A bad signature stops the install and shows why.
if output=$(MOCK_COSIGN_REJECTS=1 install_without_gh "$NO_GH_BIN"); then
  echo "installer accepted a release whose signature did not verify" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -F "none of the expected identities matched" >/dev/null
[ ! -e "$INSTALL_DIR/azud" ]

# A release from before signed checksums says what it needs.
if output=$(MOCK_NO_BUNDLE=1 install_without_gh "$NO_GH_BIN"); then
  echo "installer accepted a release with no signed checksums" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -F "has no signed checksums" >/dev/null
[ ! -e "$INSTALL_DIR/azud" ]

# A cosign already on the path is used instead of downloading one.
COSIGN_BIN="$TEST_TMP/cosign-bin"
mkdir -p "$COSIGN_BIN"
cp "$MOCK_COSIGN" "$COSIGN_BIN/cosign"
output=$(install_without_gh "$COSIGN_BIN:$NO_GH_BIN")
if printf '%s\n' "$output" | grep -F "downloading cosign" >/dev/null; then
  echo "installer downloaded cosign although one is on the path" >&2
  exit 1
fi
grep -F "verify-blob" "$MOCK_COSIGN_ARGS" >/dev/null
[ -x "$INSTALL_DIR/azud" ]

# A moving major action tag must still install binaries released before the
# stable version --short surface existed.
LEGACY_INSTALL_DIR="$TEST_TMP/legacy-install"
legacy_output=$(
  PATH="$MOCK_BIN:$PATH" \
  AZUD_VERSION=v1.2.3 \
  AZUD_INSTALL_DIR="$LEGACY_INSTALL_DIR" \
  MOCK_BINARY="$MOCK_BINARY" \
  MOCK_GH_ARGS="$MOCK_GH_ARGS" \
  MOCK_LEGACY_VERSION=1 \
  sh scripts/install.sh
)
printf '%s\n' "$legacy_output" | grep -F "  OK     azud v1.2.3" >/dev/null

if grep -E -- 'login .* (-p|--password)([=[:space:]]|$)' action.yml >/dev/null; then
  echo "registry password is passed through process arguments" >&2
  exit 1
fi
grep -F -- '--password-stdin' action.yml >/dev/null

install_script=$(awk '
  $0 == "    - name: Install Azud" { in_step = 1 }
  in_step && $0 == "      run: |" { capture = 1; next }
  capture && /^        / { sub(/^        /, ""); print; next }
  capture { exit }
' action.yml | sed 's/\$HOME/\$ACTION_TEST_HOME/g')
test -n "$install_script"
ACTION_HOME="$TEST_TMP/action-home"
ACTION_GITHUB_PATH="$TEST_TMP/github-path"
ACTION_GITHUB_OUTPUT="$TEST_TMP/github-output"
mkdir -p "$ACTION_HOME"
PATH="$MOCK_BIN:$PATH" \
ACTION_TEST_HOME="$ACTION_HOME" \
ACTION_PATH="$(pwd)" \
AZUD_VERSION=v1.2.3 \
AZUD_INSTALL_DIR="$ACTION_HOME/.azud/bin" \
GITHUB_PATH="$ACTION_GITHUB_PATH" \
GITHUB_OUTPUT="$ACTION_GITHUB_OUTPUT" \
MOCK_BINARY="$MOCK_BINARY" \
MOCK_GH_ARGS="$MOCK_GH_ARGS" \
MOCK_LEGACY_VERSION=1 \
bash -c "$install_script" >/dev/null
grep -Fx "$ACTION_HOME/.azud/bin" "$ACTION_GITHUB_PATH" >/dev/null
grep -Fx "version=v1.2.3" "$ACTION_GITHUB_OUTPUT" >/dev/null

cat > "$MOCK_BIN/podman" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" > "$MOCK_REGISTRY_ARGS"
cat > "$MOCK_REGISTRY_STDIN"
EOF
chmod +x "$MOCK_BIN/podman"

# Execute the registry-login script extracted from the composite action. This
# checks the runner-visible argv and stdin behavior of the actual action code,
# rather than only matching its YAML text.
login_script=$(awk '
  $0 == "    - name: Login to container registry" { in_step = 1 }
  in_step && $0 == "      run: |" { capture = 1; next }
  capture && /^        / { sub(/^        /, ""); print; next }
  capture { exit }
' action.yml)
test -n "$login_script"
registry_secret='runner-secret-not-in-argv'
PATH="$MOCK_BIN:$PATH" \
REGISTRY_SERVER=registry.example.test \
REGISTRY_USERNAME=integration \
REGISTRY_PASSWORD="$registry_secret" \
MOCK_REGISTRY_ARGS="$MOCK_REGISTRY_ARGS" \
MOCK_REGISTRY_STDIN="$MOCK_REGISTRY_STDIN" \
bash -c "$login_script"

grep -F -- '--password-stdin' "$MOCK_REGISTRY_ARGS" >/dev/null
if grep -F -- "$registry_secret" "$MOCK_REGISTRY_ARGS" >/dev/null; then
  echo "registry password appeared in the spawned process arguments" >&2
  exit 1
fi
[ "$(cat "$MOCK_REGISTRY_STDIN")" = "$registry_secret" ]

printf '  PASS   release installer and registry-login smoke\n'
