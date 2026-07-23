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
