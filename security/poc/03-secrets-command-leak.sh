#!/usr/bin/env bash
# JIT-05: failing secrets_command output is echoed in the error.
set -euo pipefail
AZUD_BIN=${AZUD_BIN:-azud}
command -v "$AZUD_BIN" >/dev/null || { echo "set AZUD_BIN to the azud binary" >&2; exit 1; }

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/config" "$T/.azud"
cat >"$T/config/deploy.yml" <<'YAML'
service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: command
secrets_command: |
  printf 'DB_PASS=hunter2\n'
  exit 1
YAML

set +e
OUT=$(cd "$T" && "$AZUD_BIN" -c config/deploy.yml config 2>&1)
RC=$?
set -e
printf 'exit=%s\n%s\n' "$RC" "$OUT"
if [[ "$OUT" == *hunter2* ]]; then
  printf 'VULNERABLE: secret value appeared in the error output\n'
else
  printf 'secret was not echoed\n'
fi
