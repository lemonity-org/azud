#!/bin/sh
set -eu

runtime=${CONTAINER_RUNTIME:-docker}
image=${CADDY_CONTRACT_IMAGE:-caddy:2.11.4}
name="azud-caddy-contract-$$"
volume="${name}-data"
config_file=$(mktemp)

cleanup() {
  "$runtime" rm -f "$name" >/dev/null 2>&1 || true
  "$runtime" volume rm "$volume" >/dev/null 2>&1 || true
  rm -f "$config_file"
}
trap cleanup EXIT INT TERM

"$runtime" volume create "$volume" >/dev/null
"$runtime" run -d --name "$name" \
  -p 127.0.0.1::2019 \
  -e CADDY_ADMIN=0.0.0.0:2019 \
  -v "$volume:/config" \
  "$image" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume >/dev/null

admin_port=$("$runtime" port "$name" 2019/tcp | awk -F: 'NR == 1 { print $NF }')
admin_url="http://127.0.0.1:$admin_port"
ready=false
attempt=0
while [ "$attempt" -lt 100 ]; do
  if curl -fsS "$admin_url/config/" >/dev/null 2>&1; then
    ready=true
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.1
done
[ "$ready" = true ]

cat >"$config_file" <<'JSON'
{"admin":{"listen":"0.0.0.0:2019"},"apps":{"http":{"servers":{"srv0":{"listen":[":8080"],"automatic_https":{"disable_redirects":true,"disable_certificates":true},"max_header_bytes":2097152,"enable_full_duplex":true,"routes":[{"@id":"manual-status","handle":[{"handler":"static_response","status_code":"{http.error.status_code}"}]},{"@id":"azud-route-hooks","match":[{"host":["hook.events"]}],"handle":[{"handler":"encode","encodings":{"gzip":{}}},{"@id":"azud-proxy-hooks","handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:4000"}],"flush_interval":"-1s","stream_close_delay":"5m"}]}]}}}}}
JSON
curl -fsS -X POST -H 'Content-Type: application/json' --data-binary @"$config_file" "$admin_url/load"

put_code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H 'Content-Type: application/json' \
  --data '[{"dial":"127.0.0.1:4999"}]' \
  "$admin_url/id/azud-proxy-hooks/upstreams")
[ "$put_code" = 409 ]

curl -fsS -X PATCH -H 'Content-Type: application/json' \
  --data '[{"dial":"127.0.0.1:4001"}]' \
  "$admin_url/id/azud-proxy-hooks/upstreams"
curl -fsS -X PATCH -H 'Content-Type: application/json' \
  --data '[{"host":["hook.events","*.hook.events"]}]' \
  "$admin_url/id/azud-route-hooks/match"
curl -fsS -X DELETE "$admin_url/config/apps/http/servers/srv0/max_header_bytes"

before=$(curl -fsS "$admin_url/config/")
for expected in \
  '"status_code":"{http.error.status_code}"' \
  '"handler":"encode"' \
  '"dial":"127.0.0.1:4001"' \
  '"*.hook.events"' \
  '"enable_full_duplex":true' \
  '"disable_certificates":true'
do
  printf '%s' "$before" | grep -F "$expected" >/dev/null
done

"$runtime" stop "$name" >/dev/null
"$runtime" rm "$name" >/dev/null
"$runtime" run -d --name "$name" \
  -p "127.0.0.1:$admin_port:2019" \
  -e CADDY_ADMIN=0.0.0.0:2019 \
  -v "$volume:/config" \
  "$image" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume >/dev/null

ready=false
attempt=0
while [ "$attempt" -lt 100 ]; do
  if after=$(curl -fsS "$admin_url/config/" 2>/dev/null); then
    ready=true
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.1
done
[ "$ready" = true ]
[ "$before" = "$after" ]

printf 'PASS Caddy 2.11.4 admin verbs, arbitrary JSON preservation, and --resume persistence\n'
