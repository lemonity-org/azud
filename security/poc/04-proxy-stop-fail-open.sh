#!/usr/bin/env bash
# JIT-06: proxy stop reports success when the host is unreachable.
set -euo pipefail
AZUD_BIN=${AZUD_BIN:-azud}
command -v "$AZUD_BIN" >/dev/null || { echo "set AZUD_BIN to the azud binary" >&2; exit 1; }
command -v ssh-keygen >/dev/null || { echo "ssh-keygen is required" >&2; exit 1; }

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/config" "$T/.azud"
ssh-keygen -q -t ed25519 -N '' -f "$T/key"
cat >"$T/config/deploy.yml" <<YAML
service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
ssh:
  user: $(id -un)
  port: 1
  keys: ["$T/key"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
YAML

set +e
OUT=$(cd "$T" && "$AZUD_BIN" -c config/deploy.yml proxy stop 2>&1)
RC=$?
set -e
printf 'exit=%s\n%s\n' "$RC" "$OUT"
if [[ "$RC" == "0" && "$OUT" == *"Proxy stopped"* ]]; then
  printf 'VULNERABLE: proxy stop exited 0 and claimed success after connection failure\n'
else
  printf 'proxy stop failed closed\n'
fi
