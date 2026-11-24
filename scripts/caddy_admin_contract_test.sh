#!/bin/sh
# Live contract for the Caddy image Azud pins. It boots Caddy with exactly the
# proxy flags Azud uses and checks that:
#   - the image ships curl, which Azud execs to reach the admin API;
#   - the admin API answers only on the private unix socket (every verb Azud
#     sends goes through `exec ... curl --unix-socket`);
#   - a peer container on the same network cannot reach TCP 2019 even though
#     it reaches Caddy's HTTP listener;
#   - the hardened flags (read-only rootfs, all capabilities dropped except
#     NET_BIND_SERVICE, no-new-privileges) still boot;
#   - --resume keeps arbitrary JSON and the socket listener across restarts;
#   - an autosave's admin.listen overrides CADDY_ADMIN, which is why Azud
#     normalizes the autosave before every start, and the normalized autosave
#     brings the listener back to the socket;
#   - the configuration Azud generates for proxy.on_demand_tls
#     (internal/proxy/testdata/on_demand_contract.json) loads; plain HTTP
#     for a host no route names is redirected to HTTPS; an unknown host gets
#     a certificate during the handshake only when the app's permission
#     check allows it, which Caddy calls without X-Forwarded-For while
#     proxied requests carry it; PUT on a routes index inserts before the
#     host-less route; the permission endpoint can be patched; and an
#     on-demand certificate survives a restart instead of being obtained
#     again.
set -eu

if [ -n "${CONTAINER_RUNTIME:-}" ]; then
  runtime=$CONTAINER_RUNTIME
elif command -v docker >/dev/null 2>&1; then
  runtime=docker
else
  runtime=podman
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
pinned_image=$(sed -n 's/^[[:space:]]*CaddyImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$script_dir/../internal/proxy/manager.go")
if [ -z "$pinned_image" ]; then
  printf 'FAIL could not read proxy.CaddyImage from internal/proxy/manager.go\n' >&2
  exit 1
fi
image=${CADDY_CONTRACT_IMAGE:-$pinned_image}

prefix=${CADDY_CONTRACT_PREFIX:-azud-caddy-contract}
name="$prefix-$$"
peer="$name-peer"
app="$name-app"
network="$name-net"
data_volume="$name-data"
config_volume="$name-config"
config_file=$(mktemp)

socket=/run/azud-admin/admin.sock
admin_listen="unix/$socket|0600"
admin_tmpfs=/run/azud-admin:rw,nosuid,nodev,noexec,mode=0700,size=64k

cleanup() {
  "$runtime" rm -f "$name" "$peer" "$app" >/dev/null 2>&1 || true
  "$runtime" network rm "$network" >/dev/null 2>&1 || true
  "$runtime" volume rm "$data_volume" "$config_volume" >/dev/null 2>&1 || true
  rm -f "$config_file"
}
trap cleanup EXIT INT TERM

fail() {
  printf 'FAIL %s\n' "$*" >&2
  exit 1
}

run_proxy() {
  "$runtime" run -d --name "$name" \
    --network "$network" \
    -e "CADDY_ADMIN=$admin_listen" \
    -v "$data_volume:/data" \
    -v "$config_volume:/config" \
    --read-only \
    --cap-drop all \
    --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges \
    --tmpfs "$admin_tmpfs" \
    "$image" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume >/dev/null
}

admin() {
  "$runtime" exec "$name" curl -sSf --max-time 30 --unix-socket "$socket" "$@"
}

admin_with_body() {
  "$runtime" exec -i "$name" curl -sSf --max-time 30 --unix-socket "$socket" \
    -H 'Content-Type: application/json' --data-binary @- "$@"
}

wait_for_admin() {
  attempt=0
  while [ "$attempt" -lt 100 ]; do
    if admin http://localhost/config/ >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
  done
  "$runtime" logs "$name" >&2 || true
  fail "admin socket did not become ready"
}

# peer_curl runs curl from another container on the proxy's network.
peer_curl() {
  "$runtime" run --rm --name "$peer" --network "$network" --entrypoint curl "$image" "$@"
}

assert_peer_cannot_reach_admin() {
  if peer_curl -sS -m 3 "http://$name:2019/config/" >/dev/null 2>&1; then
    fail "$1: a peer container reached the admin API on $name:2019"
  fi
}

write_autosave() {
  "$runtime" run --rm -i -v "$config_volume:/config" --entrypoint /bin/sh "$image" \
    -c 'mkdir -p /config/caddy && cat > /config/caddy/autosave.json'
}

"$runtime" volume create "$data_volume" >/dev/null
"$runtime" volume create "$config_volume" >/dev/null
"$runtime" network create "$network" >/dev/null

"$runtime" run --rm --entrypoint /bin/sh "$image" -c 'command -v curl >/dev/null && curl --version | grep -qi unix' ||
  fail "$image does not ship a curl with unix socket support"

run_proxy
wait_for_admin

cat >"$config_file" <<JSON
{"admin":{"listen":"$admin_listen"},"apps":{"http":{"servers":{"srv0":{"listen":[":8080"],"automatic_https":{"disable_redirects":true,"disable_certificates":true},"max_header_bytes":2097152,"enable_full_duplex":true,"routes":[{"@id":"manual-status","handle":[{"handler":"static_response","status_code":"{http.error.status_code}"}]},{"@id":"azud-route-hooks","match":[{"host":["hook.events"]}],"handle":[{"handler":"encode","encodings":{"gzip":{}}},{"@id":"azud-proxy-hooks","handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:4000"}],"flush_interval":"-1s","stream_close_delay":"5m"}]}]}}}}}
JSON
admin_with_body -X POST http://localhost/load <"$config_file"

put_code=$(printf '%s' '[{"dial":"127.0.0.1:4999"}]' | "$runtime" exec -i "$name" curl -sS -o /dev/null -w '%{http_code}' \
  --max-time 30 --unix-socket "$socket" -X PUT -H 'Content-Type: application/json' --data-binary @- \
  http://localhost/id/azud-proxy-hooks/upstreams)
[ "$put_code" = 409 ] || fail "PUT on an existing array returned $put_code, want 409"

printf '%s' '[{"dial":"127.0.0.1:4001"}]' | admin_with_body -X PATCH http://localhost/id/azud-proxy-hooks/upstreams
printf '%s' '[{"host":["hook.events","*.hook.events"]}]' | admin_with_body -X PATCH http://localhost/id/azud-route-hooks/match
admin -X DELETE http://localhost/config/apps/http/servers/srv0/max_header_bytes

before=$(admin http://localhost/config/)
for expected in \
  '"status_code":"{http.error.status_code}"' \
  '"handler":"encode"' \
  '"dial":"127.0.0.1:4001"' \
  '"*.hook.events"' \
  '"enable_full_duplex":true' \
  '"disable_certificates":true' \
  "\"listen\":\"unix/$socket|0600\""
do
  printf '%s' "$before" | grep -F "$expected" >/dev/null || fail "live config is missing $expected"
done

# The data plane is reachable from a peer, so the refused admin connection is
# not an artifact of network isolation.
http_code=$(peer_curl -sS -m 3 -o /dev/null -w '%{http_code}' "http://$name:8080/") ||
  fail "peer could not reach Caddy's HTTP listener"
[ "$http_code" != 000 ] || fail "peer could not reach Caddy's HTTP listener"
assert_peer_cannot_reach_admin "hardened proxy"

readonly_rootfs=$("$runtime" container inspect "$name" --format '{{.HostConfig.ReadonlyRootfs}}')
[ "$readonly_rootfs" = true ] || fail "proxy root filesystem is not read-only"
"$runtime" exec "$name" sh -c 'grep -q "^NoNewPrivs:[[:space:]]*1" /proc/1/status' ||
  fail "proxy does not run with no-new-privileges"
cap_eff=$("$runtime" exec "$name" sh -c 'sed -n "s/^CapEff:[[:space:]]*//p" /proc/1/status')
[ "$cap_eff" = 0000000000000400 ] || fail "proxy effective capabilities are $cap_eff, want only NET_BIND_SERVICE"

"$runtime" stop "$name" >/dev/null
"$runtime" rm "$name" >/dev/null
run_proxy
wait_for_admin
after=$(admin http://localhost/config/)
[ "$before" = "$after" ] || fail "--resume did not restore the same live config"
assert_peer_cannot_reach_admin "resumed proxy"

# An autosave written by an older release listens on TCP. Caddy lets it
# override CADDY_ADMIN under --resume, so Azud must rewrite it before start.
"$runtime" stop "$name" >/dev/null
"$runtime" rm "$name" >/dev/null
legacy=$(printf '%s' "$after" | sed "s#\"listen\":\"unix/$socket|0600\"#\"listen\":\"0.0.0.0:2019\"#")
printf '%s' "$legacy" | grep -F '"listen":"0.0.0.0:2019"' >/dev/null || fail "could not build a legacy autosave"
printf '%s' "$legacy" | write_autosave
run_proxy
attempt=0
until peer_curl -fsS -m 3 "http://$name:2019/config/" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 50 ] || fail "a legacy autosave no longer overrides CADDY_ADMIN; review Azud's autosave normalization"
  sleep 0.1
done

# Azud's normalization replaces the whole admin object with the socket
# listener; Caddy then resumes on the socket only.
"$runtime" stop "$name" >/dev/null
"$runtime" rm "$name" >/dev/null
printf '%s' "$after" | write_autosave
run_proxy
wait_for_admin
assert_peer_cannot_reach_admin "normalized autosave"
normalized=$(admin http://localhost/config/)
[ "$normalized" = "$after" ] || fail "normalized autosave did not restore the same live config"

# On-demand TLS. The app answers the permission check on app:3000/tls/ask:
# yes for allowed.example.com when the request has no X-Forwarded-For, as
# Caddy's own check has none, and no otherwise. Every other request echoes
# the host and X-Forwarded-For.
app_config='{"admin":{"disabled":true},"apps":{"http":{"servers":{"app":{"listen":[":3000"],"automatic_https":{"disable":true},"routes":[
{"match":[{"path":["/tls/ask"],"query":{"domain":["allowed.example.com"]},"header":{"X-Forwarded-For":null}}],"handle":[{"handler":"static_response","status_code":200,"body":"yes"}]},
{"match":[{"path":["/tls/ask"]}],"handle":[{"handler":"static_response","status_code":404,"body":"no"}]},
{"handle":[{"handler":"static_response","body":"app {http.request.host} xff={http.request.header.X-Forwarded-For}"}]}]}}}}}'
"$runtime" run -d --name "$app" --network "$network" --network-alias app -e "APP_CONFIG=$app_config" \
  --entrypoint /bin/sh "$image" -c 'printf "%s" "$APP_CONFIG" > /tmp/app.json && exec caddy run --config /tmp/app.json' >/dev/null

# The fixture's ACME issuer is swapped for Caddy's internal CA, so the
# contract needs no public DNS.
on_demand_fixture="$script_dir/../internal/proxy/testdata/on_demand_contract.json"
[ -f "$on_demand_fixture" ] || fail "missing $on_demand_fixture"
sed -e 's/"module": "acme"/"module": "internal"/' -e '/"email": /d' "$on_demand_fixture" >"$config_file"
grep -F '"on_demand": true' "$config_file" >/dev/null || fail "the on-demand fixture has no on-demand policy"
admin_with_body -X POST http://localhost/load <"$config_file" >/dev/null ||
  fail "Caddy refused the configuration Azud generates for on-demand TLS"

# https_get HOST PATH [curl flags]: HTTPS to the proxy with HOST as SNI.
https_get() {
  host=$1
  path=$2
  shift 2
  peer_curl -sS -k -m 10 --connect-to "$host:443:$name:443" "$@" "https://$host$path"
}

attempt=0
until known=$(https_get app.example.com /known 2>/dev/null); do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 50 ] || fail "app.example.com was not served over HTTPS"
  sleep 0.2
done
case "$known" in "app app.example.com xff="?*) ;; *) fail "app.example.com answered $known" ;; esac

redirect=$(peer_curl -sS -m 5 -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: unknown.example.net' "http://$name/path?x=1")
[ "$redirect" = "308 https://unknown.example.net/path?x=1" ] ||
  fail "plain HTTP for a host no route names answered '$redirect', want a redirect to HTTPS"

allowed=$(https_get allowed.example.com /hello) || fail "allowed.example.com was not served over HTTPS"
case "$allowed" in "app allowed.example.com xff="?*) ;; *) fail "allowed.example.com answered $allowed" ;; esac

if https_get denied.example.com / >/dev/null 2>&1; then
  fail "denied.example.com completed a handshake although the permission check refused it"
fi

proxied=$(https_get allowed.example.com '/tls/ask?domain=allowed.example.com' -o /dev/null -w '%{http_code}')
[ "$proxied" = 404 ] || fail "a proxied permission check answered $proxied; proxied requests must carry X-Forwarded-For"

printf '%s' '{"@id":"azud-route-docs","match":[{"host":["docs.example.org"]}],"handle":[{"handler":"static_response","body":"docs"}],"terminal":true}' |
  admin_with_body -X PUT http://localhost/config/apps/http/servers/srv0/routes/2 >/dev/null
order=$(admin http://localhost/config/apps/http/servers/srv0/routes | grep -o '"@id":"azud-\(route\|on-demand\)-[a-z]*"' | tr '\n' ' ')
[ "$order" = '"@id":"azud-route-blog" "@id":"azud-route-app" "@id":"azud-route-docs" "@id":"azud-on-demand-app" ' ] ||
  fail "PUT on a routes index did not insert before the on-demand route: $order"

printf '%s' '"http://app:3000/tls/ask?moved=1"' |
  admin_with_body -X PATCH http://localhost/config/apps/tls/automation/on_demand/permission/endpoint >/dev/null
admin http://localhost/config/apps/tls | grep -F '"endpoint":"http://app:3000/tls/ask?moved=1"' >/dev/null ||
  fail "the on-demand permission endpoint could not be patched"

issued=$(https_get allowed.example.com / -v 2>&1 | sed -n 's/^.*start date: //p')
[ -n "$issued" ] || fail "could not read the on-demand certificate"
"$runtime" stop "$name" >/dev/null
"$runtime" rm "$name" >/dev/null
run_proxy
wait_for_admin
reissued=$(https_get allowed.example.com / -v 2>&1 | sed -n 's/^.*start date: //p')
[ "$reissued" = "$issued" ] ||
  fail "the on-demand certificate was obtained again after a restart ($issued, then $reissued); Caddy's /data must persist"

printf 'PASS %s: private admin socket, hardened flags, admin verbs, arbitrary JSON preservation, --resume persistence, and on-demand TLS (%s)\n' "$image" "$runtime"
