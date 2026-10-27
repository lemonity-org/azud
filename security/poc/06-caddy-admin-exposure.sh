#!/usr/bin/env bash
# JIT-01: probe the Caddy admin API from a peer container on the same network.
#
# This script mutates a disposable container runtime and must be run only on a
# test host. It skips cleanly when no working runtime is available.
set -euo pipefail

for candidate in podman docker; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" info >/dev/null 2>&1; then
    RT=$candidate
    break
  fi
done
if [[ -z "${RT:-}" ]]; then
  echo "SKIP: no working podman/docker daemon; see security/SECURITY_FINDINGS.md for the live command"
  exit 0
fi

NET=azud-jit-$$
PROXY=azud-proxy-jit-$$
cleanup() {
  "$RT" rm -f "$PROXY" >/dev/null 2>&1 || true
  "$RT" network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

"$RT" network create "$NET" >/dev/null
"$RT" run -d --name "$PROXY" --network "$NET" \
  -e CADDY_ADMIN=0.0.0.0:2019 \
  -p 127.0.0.1:0:2019 \
  docker.io/library/caddy:2.11.4-alpine \
  caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume >/dev/null

# Wait for Caddy to initialize.
for _ in $(seq 1 50); do
  if "$RT" run --rm --network "$NET" docker.io/library/alpine:3.23 \
      sh -c "wget -qO- http://$PROXY:2019/config/ >/dev/null 2>&1"; then
    break
  fi
  sleep 0.2
done

BODY=$("$RT" run --rm --network "$NET" docker.io/library/alpine:3.23 \
  sh -c "wget -qO- http://$PROXY:2019/config/ 2>/dev/null | head -c 200") || true
printf 'admin response from peer container: %s\n' "${BODY:-<empty>}"
if [[ -n "${BODY:-}" ]]; then
  printf 'VULNERABLE: a peer container reached the unauthenticated Caddy admin API\n'
else
  printf 'peer container could not read the admin API\n'
fi
