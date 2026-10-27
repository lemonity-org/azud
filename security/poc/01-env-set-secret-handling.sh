#!/usr/bin/env bash
# JIT-04: `azud env set` preserves an existing 0644 mode and follows symlinks.
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

printf 'OLD=value\n' >"$T/.azud/secrets"
chmod 0644 "$T/.azud/secrets"
(
  cd "$T"
  "$AZUD_BIN" -c config/deploy.yml env set NEW value >/dev/null
)
mode=$(stat -c '%a' "$T/.azud/secrets")
printf 'secrets mode after env set: %s\n' "$mode"
if [[ "$mode" == "644" ]]; then
  printf 'VULNERABLE: existing 0644 mode was preserved\n'
else
  printf 'mode was tightened to %s\n' "$mode"
fi

printf 'ORIGINAL\n' >"$T/victim.txt"
chmod 0600 "$T/victim.txt"
rm -f "$T/.azud/secrets"
ln -s "$T/victim.txt" "$T/.azud/secrets"
(
  cd "$T"
  "$AZUD_BIN" -c config/deploy.yml env set PWNED yes >/dev/null
)
if grep -q 'PWNED=yes' "$T/victim.txt"; then
  printf 'VULNERABLE: .azud/secrets symlink was followed and victim.txt was overwritten\n'
else
  printf 'symlink was not followed\n'
fi
