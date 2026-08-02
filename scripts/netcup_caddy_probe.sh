#!/usr/bin/env bash
# Run one disposable rootless-Podman Caddy compatibility probe through a single
# SSH connection as PaceBeats' real deploy user. The probe never touches
# azud-proxy or the production Caddy volumes/network. Its uniquely named
# container, volumes, network, and temporary evidence file are removed by an
# EXIT trap.
#
# Usage:
#   scripts/netcup_caddy_probe.sh deploy@<host-or-ssh-alias>
#   scripts/netcup_caddy_probe.sh --via-operator <operator-ssh-alias>
#
# The immutable Caddy image may remain in Podman's cache. Keeping a shared image
# is safer than deleting one that an existing or concurrently created proxy may
# use. This script is intentionally separate from the live systemd handoff probe.
set -euo pipefail

remote_command='bash -s'
case "$#:${1-}" in
	1:*) ssh_target="$1" ;;
	2:--via-operator)
		ssh_target="$2"
		# The operator is only a transport. The complete probe runs in deploy's
		# login environment and independently rejects any non-deploy identity.
		remote_command='sudo -n -iu deploy env XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus bash -s'
		;;
	*)
		echo "usage: $0 [--via-operator] <ssh-config-host-or-user@host>" >&2
		exit 2
		;;
esac

case "$ssh_target" in
	-* | *$'\n'* | *$'\r'*)
		echo "invalid SSH target" >&2
		exit 2
		;;
esac

# One SSH process means one 1Password SSH-agent authorization window.
ssh -T "$ssh_target" "$remote_command" <<'AZUD_NETCUP_PROBE'
set -euo pipefail
export LC_ALL=C
umask 077

readonly caddy_image='docker.io/library/caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648'
readonly admin_listen='127.0.0.1:2019'
readonly runtime_revision='2'
readonly probe_id="azud-caddy-probe-$(id -u)-$$-$(date +%s)"
probe_owner=""
if ! IFS= read -r probe_owner </proc/sys/kernel/random/uuid || [ -z "$probe_owner" ]; then
	echo "FAIL: cannot generate the disposable probe ownership token" >&2
	exit 1
fi
readonly probe_owner
readonly container="${probe_id}"
readonly writer="${probe_id}-writer"
readonly upstream="${probe_id}-upstream"
readonly upstream_alias="${probe_id}-backend-alias"
readonly network="${probe_id}-network"
readonly data_volume="${probe_id}-data"
readonly config_volume="${probe_id}-config"
inspect_file=""
container_ref=""
writer_ref=""
upstream_ref=""
network_ref=""
network_id=""
data_volume_ref=""
config_volume_ref=""
container_attempted=0
writer_attempted=0
upstream_attempted=0
network_attempted=0
data_volume_attempted=0
config_volume_attempted=0

if ! command -v podman >/dev/null 2>&1; then
	echo "FAIL: podman is not installed" >&2
	exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
	echo "FAIL: host curl is required for the loopback ingress check" >&2
	exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
	echo "FAIL: python3 is required to validate Podman inspect JSON" >&2
	exit 1
fi
if ! command -v nproc >/dev/null 2>&1; then
	echo "FAIL: nproc is required for the host capacity evidence" >&2
	exit 1
fi

if [ "$(id -un)" != deploy ] || [ "$(id -u)" -eq 0 ]; then
	echo "FAIL: run the probe through the non-root PaceBeats deploy identity" >&2
	exit 1
fi
if [ "$(podman info --format '{{.Host.Security.Rootless}}')" != true ]; then
	echo "FAIL: deploy user's Podman runtime is not rootless" >&2
	exit 1
fi

p() { podman "$@"; }

container_has_exact_ownership() {
	local reference="$1"
	local expected_id="$2"
	local actual_id actual_probe actual_owner
	actual_id="$(p container inspect "$reference" --format '{{.Id}}' 2>/dev/null)" || return 1
	actual_probe="$(p container inspect "$reference" --format '{{ index .Config.Labels "io.azud.probe" }}' 2>/dev/null)" || return 1
	actual_owner="$(p container inspect "$reference" --format '{{ index .Config.Labels "io.azud.probe.owner" }}' 2>/dev/null)" || return 1
	[ -z "$expected_id" ] || [ "$actual_id" = "$expected_id" ] || return 1
	[ "$actual_probe" = "$probe_id" ] && [ "$actual_owner" = "$probe_owner" ]
}

volume_has_exact_ownership() {
	local name="$1"
	local actual_probe actual_owner
	actual_probe="$(p volume inspect "$name" --format '{{ index .Labels "io.azud.probe" }}' 2>/dev/null)" || return 1
	actual_owner="$(p volume inspect "$name" --format '{{ index .Labels "io.azud.probe.owner" }}' 2>/dev/null)" || return 1
	[ "$actual_probe" = "$probe_id" ] && [ "$actual_owner" = "$probe_owner" ]
}

network_has_exact_ownership() {
	local name="$1"
	local expected_id="$2"
	local actual_id actual_probe actual_owner
	actual_id="$(p network inspect "$name" --format '{{.ID}}' 2>/dev/null)" || return 1
	actual_probe="$(p network inspect "$name" --format '{{ index .Labels "io.azud.probe" }}' 2>/dev/null)" || return 1
	actual_owner="$(p network inspect "$name" --format '{{ index .Labels "io.azud.probe.owner" }}' 2>/dev/null)" || return 1
	[ -z "$expected_id" ] || [ "$actual_id" = "$expected_id" ] || return 1
	[ "$actual_probe" = "$probe_id" ] && [ "$actual_owner" = "$probe_owner" ]
}

remove_owned_container() {
	local name="$1"
	local reference="$2"
	local attempted="$3"
	local target expected_id actual_id
	[ "$attempted" -eq 1 ] || return 0
	if [ -n "$reference" ] && p container exists "$reference" >/dev/null 2>&1; then
		target="$reference"
		expected_id="$reference"
	elif p container exists "$name" >/dev/null 2>&1; then
		target="$name"
		expected_id=""
	else
		return 0
	fi
	if ! container_has_exact_ownership "$target" "$expected_id"; then
		echo "FAIL: refusing to delete container $name without its exact probe ID and ownership labels" >&2
		cleanup_failed=1
		return 0
	fi
	actual_id="$(p container inspect "$target" --format '{{.Id}}' 2>/dev/null)"
	if [ -z "$actual_id" ] || ! p rm -f "$actual_id" >/dev/null 2>&1; then
		echo "FAIL: could not remove owned container $name" >&2
		cleanup_failed=1
		return 0
	fi
	if p container exists "$actual_id" >/dev/null 2>&1; then
		echo "FAIL: cleanup left owned container $name" >&2
		cleanup_failed=1
	fi
}

remove_owned_volume() {
	local name="$1"
	local reference="$2"
	local attempted="$3"
	[ "$attempted" -eq 1 ] || return 0
	if ! p volume exists "$name" >/dev/null 2>&1; then
		return 0
	fi
	if ! volume_has_exact_ownership "$name"; then
		echo "FAIL: refusing to delete volume $name without its exact probe ownership labels" >&2
		cleanup_failed=1
		return 0
	fi
	if ! p volume rm "$name" >/dev/null 2>&1; then
		echo "FAIL: could not remove owned volume $name" >&2
		cleanup_failed=1
		return 0
	fi
	if p volume exists "$name" >/dev/null 2>&1; then
		echo "FAIL: cleanup left owned volume $name" >&2
		cleanup_failed=1
	fi
}

remove_owned_network() {
	local name="$1"
	local reference="$2"
	local expected_id="$3"
	local attempted="$4"
	[ "$attempted" -eq 1 ] || return 0
	if ! p network exists "$name" >/dev/null 2>&1; then
		return 0
	fi
	if ! network_has_exact_ownership "$name" "$expected_id"; then
		echo "FAIL: refusing to delete network $name without its exact probe ID and ownership labels" >&2
		cleanup_failed=1
		return 0
	fi
	if ! p network rm "$name" >/dev/null 2>&1; then
		echo "FAIL: could not remove owned network $name" >&2
		cleanup_failed=1
		return 0
	fi
	if p network exists "$name" >/dev/null 2>&1; then
		echo "FAIL: cleanup left owned network $name" >&2
		cleanup_failed=1
	fi
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	set +e

	[ -z "$inspect_file" ] || rm -f -- "$inspect_file"
	cleanup_failed=0
	remove_owned_container "$container" "$container_ref" "$container_attempted"
	remove_owned_container "$writer" "$writer_ref" "$writer_attempted"
	remove_owned_container "$upstream" "$upstream_ref" "$upstream_attempted"
	remove_owned_volume "$data_volume" "$data_volume_ref" "$data_volume_attempted"
	remove_owned_volume "$config_volume" "$config_volume_ref" "$config_volume_attempted"
	remove_owned_network "$network" "$network_ref" "$network_id" "$network_attempted"
	if [ "$cleanup_failed" -ne 0 ] && [ "$status" -eq 0 ]; then
		status=1
	fi
	if [ "$cleanup_failed" -eq 0 ] && { [ "$container_attempted" -eq 1 ] || [ "$writer_attempted" -eq 1 ] || [ "$upstream_attempted" -eq 1 ] || [ "$network_attempted" -eq 1 ] || [ "$data_volume_attempted" -eq 1 ] || [ "$config_volume_attempted" -eq 1 ]; }; then
		echo "PASS: disposable probe resources removed (immutable image cache retained)"
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

echo "== host capacity and runtime =="
printf 'probe_id=%s\n' "$probe_id"
printf 'timestamp_utc='
date -u +%Y-%m-%dT%H:%M:%SZ
printf 'kernel='
uname -srmo
if [ -r /etc/os-release ]; then
	awk -F= '$1 == "PRETTY_NAME" {print "os=" $2}' /etc/os-release
fi
printf 'online_cpus='
getconf _NPROCESSORS_ONLN
printf 'nproc='
nproc
awk '/^(MemTotal|MemAvailable|SwapTotal):/ {print tolower($1) "=" $2 " " $3}' /proc/meminfo
if command -v free >/dev/null 2>&1; then
	free -h
fi
printf 'cgroup_filesystems='
awk '$3 == "cgroup" || $3 == "cgroup2" {printf "%s:%s ", $2, $3} END {print ""}' /proc/mounts
p --version
p info --format 'rootless={{.Host.Security.Rootless}} cgroup_manager={{.Host.CgroupManager}} cgroup_version={{.Host.CgroupsVersion}} graph_driver={{.Store.GraphDriverName}}'

if ! command -v ss >/dev/null 2>&1; then
	echo "FAIL: ss is required to prove the host-network listeners are free and loopback-only" >&2
	exit 1
fi
if ss -H -ltn | awk '$4 ~ /:8080$/ || $4 ~ /:8443$/ || $4 ~ /:2019$/ {found=1} END {exit !found}'; then
	echo "FAIL: host TCP port 8080, 8443, or 2019 is already in use; no probe resources were created" >&2
	ss -H -ltn | awk '$4 ~ /:8080$/ || $4 ~ /:8443$/ || $4 ~ /:2019$/ {print}' >&2
	exit 1
fi

collision_found=0
for name in "$writer" "$upstream" "$container"; do
	if p container exists "$name" >/dev/null 2>&1; then
		echo "FAIL: disposable container name already exists: $name" >&2
		collision_found=1
	fi
done
for name in "$data_volume" "$config_volume"; do
	if p volume exists "$name" >/dev/null 2>&1; then
		echo "FAIL: disposable volume name already exists: $name" >&2
		collision_found=1
	fi
done
if p network exists "$network" >/dev/null 2>&1; then
	echo "FAIL: disposable network name already exists: $network" >&2
	collision_found=1
fi
if [ "$collision_found" -ne 0 ]; then
	echo "FAIL: refusing to reuse or remove any pre-existing same-named probe resource" >&2
	exit 1
fi

echo "== exact image =="
p pull "$caddy_image"
p image inspect "$caddy_image" --format 'id={{.Id}} digest={{.Digest}} repo_digests={{json .RepoDigests}}'

network_attempted=1
network_ref="$(p network create --label "io.azud.probe=${probe_id}" --label "io.azud.probe.owner=${probe_owner}" "$network")"
[ -n "$network_ref" ] || { echo "FAIL: Podman did not return the created network reference" >&2; exit 1; }
network_id="$(p network inspect "$network" --format '{{.ID}}')"
if ! network_has_exact_ownership "$network" "$network_id"; then
	echo "FAIL: created network does not have the exact probe ID and ownership labels" >&2
	exit 1
fi

data_volume_attempted=1
data_volume_ref="$(p volume create --label "io.azud.probe=${probe_id}" --label "io.azud.probe.owner=${probe_owner}" "$data_volume")"
[ -n "$data_volume_ref" ] || { echo "FAIL: Podman did not return the created data-volume reference" >&2; exit 1; }
if ! volume_has_exact_ownership "$data_volume"; then
	echo "FAIL: created data volume does not have the exact probe ownership labels" >&2
	exit 1
fi

config_volume_attempted=1
config_volume_ref="$(p volume create --label "io.azud.probe=${probe_id}" --label "io.azud.probe.owner=${probe_owner}" "$config_volume")"
[ -n "$config_volume_ref" ] || { echo "FAIL: Podman did not return the created config-volume reference" >&2; exit 1; }
if ! volume_has_exact_ownership "$config_volume"; then
	echo "FAIL: created config volume does not have the exact probe ownership labels" >&2
	exit 1
fi

echo "== disposable rootless bridge DNS upstream =="
upstream_attempted=1
upstream_ref="$(p create \
	--name "$upstream" \
	--label "io.azud.probe=${probe_id}" \
	--label "io.azud.probe.owner=${probe_owner}" \
	--restart no \
	--network "$network" \
	--network-alias "$upstream_alias" \
	--read-only \
	--read-only-tmpfs=false \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--pids-limit 32 \
	--memory 64m \
	--memory-swap 64m \
	--cpus 1 \
	--shm-size 4m \
	--ulimit nofile=1024:1024 \
	--user 1000:1000 \
	--entrypoint /bin/sh \
	"$caddy_image" \
	-eu -c 'while :; do printf "HTTP/1.1 200 OK\r\nContent-Length: 20\r\nConnection: close\r\n\r\nazud-netcup-upstream" | busybox nc -l -p 8081; done')"
if ! container_has_exact_ownership "$upstream_ref" "$upstream_ref"; then
	echo "FAIL: created upstream container does not have the exact probe ID and ownership labels" >&2
	exit 1
fi
p start "$upstream_ref" >/dev/null

readonly recovery_json="{\"admin\":{\"listen\":\"127.0.0.1:2019\"},\"apps\":{\"http\":{\"servers\":{\"probe\":{\"listen\":[\":80\"],\"routes\":[{\"handle\":[{\"handler\":\"reverse_proxy\",\"upstreams\":[{\"dial\":\"${upstream_alias}:8081\"}]}]}]}}}}}"
writer_attempted=1
writer_ref="$(p create --rm -i \
	--name "$writer" \
	--label "io.azud.probe=${probe_id}" \
	--label "io.azud.probe.owner=${probe_owner}" \
	--network none \
	--read-only \
	--read-only-tmpfs=false \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--no-healthcheck \
	--pids-limit 32 \
	--memory 64m \
	--memory-swap 64m \
	--cpus 1 \
	--shm-size 4m \
	--ulimit nofile=1024:1024 \
	--user 1000:1000 \
	--volume "${config_volume}:/config:U" \
	--entrypoint /bin/sh \
	"$caddy_image" \
	-eu -c 'umask 077; mkdir -p /config/caddy; tmp="$(mktemp /config/caddy/.azud.json.XXXXXX)"; trap '\''rm -f "$tmp"'\'' EXIT HUP INT TERM; cat > "$tmp"; test -s "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" /config/caddy/azud.json; sync; trap - EXIT HUP INT TERM')"
if ! container_has_exact_ownership "$writer_ref" "$writer_ref"; then
	echo "FAIL: created writer container does not have the exact probe ID and ownership labels" >&2
	exit 1
fi
printf '%s' "$recovery_json" | p start --attach --interactive "$writer_ref"

container_attempted=1
container_ref="$(p create \
	--name "$container" \
	--label "io.azud.probe=${probe_id}" \
	--label "io.azud.probe.owner=${probe_owner}" \
	--label 'azud.managed=true' \
	--label 'azud.type=proxy' \
	--label "azud.proxy.runtime=${runtime_revision}" \
	--restart no \
	--network "$network" \
	--publish '8080:80' \
	--publish '8443:443' \
	--volume "${data_volume}:/data:U" \
	--volume "${config_volume}:/config:U" \
	--env "CADDY_ADMIN=${admin_listen}" \
	--user 1000:1000 \
	--read-only \
	--read-only-tmpfs=false \
	--cap-drop ALL \
	--cap-add NET_BIND_SERVICE \
	--security-opt no-new-privileges \
	--tmpfs '/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777' \
	--tmpfs '/run:rw,noexec,nosuid,nodev,size=8m,mode=0755' \
	--pids-limit 256 \
	--shm-size 16m \
	--memory 512m \
	--memory-swap 512m \
	--cpus 4 \
	--ulimit nofile=65536:65536 \
	--stop-timeout 30 \
	--entrypoint /bin/sh \
	"$caddy_image" \
	-eu -c 'if [ -s /config/caddy/azud.json ]; then exec caddy run --config /config/caddy/azud.json; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile; fi')"
if ! container_has_exact_ownership "$container_ref" "$container_ref"; then
	echo "FAIL: created Caddy container does not have the exact probe ID and ownership labels" >&2
	exit 1
fi
p start "$container_ref" >/dev/null

if ! p exec "$container" /bin/sh -c 'command -v curl >/dev/null'; then
	echo "FAIL: the pinned Caddy image lacks curl, which Azud requires for every admin request" >&2
	exit 1
fi
p exec "$container" curl --version | sed -n '1p'

wait_for_admin() {
	for _ in $(seq 1 50); do
		if p exec "$container" curl --silent --show-error --fail-with-body --max-time 5 --noproxy '*' --proto '=http' --url "http://${admin_listen}/config/admin/listen" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.2
	done
	return 1
}

if ! wait_for_admin; then
	echo "FAIL: Caddy admin API did not become ready" >&2
	p logs "$container" >&2 || true
	exit 1
fi

echo "== in-container confinement and Caddy exercise =="
p exec "$container" /bin/sh -eu -c '
	test "$(id -u)" = 1000
	test "$(id -g)" = 1000
	! touch /usr/azud-write-test 2>/dev/null
	test -s /config/caddy/azud.json
	test "$(stat -c %a /config/caddy/azud.json)" = 600
	grep -Eq "^NoNewPrivs:[[:space:]]+1$" /proc/1/status
	for field in CapEff CapBnd; do
		caps=$(awk -v wanted="${field}:" "\$1 == wanted {print \$2}" /proc/1/status)
		test -n "$caps"
		test $((0x$caps & 0x400)) -ne 0
		test $((0x$caps & ~0x400)) -eq 0
	done
	awk "\$2 == \"/tmp\" && \$4 ~ /noexec/ && \$4 ~ /nosuid/ && \$4 ~ /nodev/ {ok=1} END {exit !ok}" /proc/mounts
	awk "\$2 == \"/run\" && \$4 ~ /noexec/ && \$4 ~ /nosuid/ && \$4 ~ /nodev/ {ok=1} END {exit !ok}" /proc/mounts
	test "$(cat /sys/fs/cgroup/memory.max)" = 536870912
	test "$(cat /sys/fs/cgroup/pids.max)" = 256
	test "$(cat /sys/fs/cgroup/cpu.max)" = "400000 100000"
	test "$(ulimit -n)" = 65536
	test "$(curl --silent --show-error --fail-with-body --noproxy \"*\" --proto \"=http\" --url http://127.0.0.1:2019/config/admin/listen)" = "\"127.0.0.1:2019\""
'

upstream_ready=0
for _ in $(seq 1 50); do
	if p exec "$container" curl --silent --show-error --fail-with-body --max-time 5 --noproxy '*' --proto '=http' --url "http://${upstream_alias}:8081/" 2>/dev/null | grep -qx 'azud-netcup-upstream'; then
		upstream_ready=1
		break
	fi
	sleep 0.2
done
if [ "$upstream_ready" -ne 1 ]; then
	echo "FAIL: Caddy container cannot resolve and reach the unique rootless-bridge upstream alias" >&2
	p logs "$upstream" >&2 || true
	exit 1
fi

test "$(curl --silent --show-error --fail-with-body --noproxy '*' --proto '=http' --url 'http://127.0.0.1:8080/')" = 'azud-netcup-upstream'
for port in 8080 8443; do
	if ! ss -H -ltn | awk -v wanted=":${port}" '$4 ~ wanted "$" {found=1} END {exit !found}'; then
		echo "FAIL: expected rootless bridge listener on host port $port" >&2
		exit 1
	fi
done
if ss -H -ltn | awk '$4 ~ /:2019$/ {found=1} END {exit !found}'; then
	echo "FAIL: Caddy admin port 2019 is listening on the host" >&2
	exit 1
fi
if [ -n "$(p port "$container" 2019/tcp 2>/dev/null || true)" ]; then
	echo "FAIL: Caddy admin port 2019 is published" >&2
	exit 1
fi

inspect_file="$(mktemp "/tmp/${probe_id}.inspect.XXXXXX")"
p inspect "$container" > "$inspect_file"

echo "== selected Podman inspect evidence =="
python3 - "$inspect_file" "$caddy_image" "$network" "$data_volume" "$config_volume" "$runtime_revision" <<'PY'
import json
import sys

path, expected_image, expected_network, data_volume, config_volume, revision = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    payload = json.load(handle)
if len(payload) != 1:
    raise SystemExit(f"FAIL: expected one inspect object, got {len(payload)}")

item = payload[0]
config = item.get("Config") or {}
host = item.get("HostConfig") or {}
network = item.get("NetworkSettings") or {}

evidence = {
    "ImageName": item.get("ImageName"),
    "EffectiveCaps": item.get("EffectiveCaps"),
    "BoundingCaps": item.get("BoundingCaps"),
    "Config": {key: config.get(key) for key in ("Image", "User", "Env", "Labels", "Cmd", "Entrypoint", "StopTimeout")},
    "HostConfig": {key: host.get(key) for key in (
        "NetworkMode", "ReadonlyRootfs", "CapAdd", "CapDrop", "SecurityOpt",
        "PidsLimit", "Memory", "MemorySwap", "NanoCpus", "ShmSize",
        "PortBindings", "Tmpfs", "ReadonlyTmpfs", "ReadOnlyTmpfs", "Ulimits",
        "RestartPolicy",
    )},
    "NetworkSettings": {"Ports": network.get("Ports"), "Networks": network.get("Networks")},
    "Mounts": item.get("Mounts"),
    "State": {key: (item.get("State") or {}).get(key) for key in ("Running", "Status", "ExitCode", "Error")},
}
print(json.dumps(evidence, indent=2, sort_keys=True))

def require(condition, message):
    if not condition:
        raise SystemExit("FAIL: " + message)

def cap(value):
    return str(value).strip().upper().removeprefix("CAP_")

def exact_cap_set(values, wanted):
    normalized = [cap(value) for value in (values or [])]
    return len(normalized) == len(wanted) and set(normalized) == wanted

def option_set(value):
    return {part.strip() for part in str(value or "").split(",") if part.strip()}

expected_name, separator, expected_digest = expected_image.partition("@")
require(separator == "@" and expected_digest.startswith("sha256:"), "probe image constant is not digest-pinned")
last_slash = expected_name.rfind("/")
last_colon = expected_name.rfind(":")
expected_repository = expected_name[:last_colon] if last_colon > last_slash else expected_name
canonical_image = f"{expected_repository}@{expected_digest}"
allowed_images = {expected_image, canonical_image}
image_refs = [value for value in (item.get("ImageName"), config.get("Image")) if value]
require(image_refs and all(value in allowed_images for value in image_refs), f"container image is not the exact pinned repository/digest: ImageName={item.get('ImageName')!r} Config.Image={config.get('Image')!r} allowed={sorted(allowed_images)!r}")
require(config.get("User") == "1000:1000", "Config.User is not 1000:1000")
require([value for value in (config.get("Env") or []) if value.startswith("CADDY_ADMIN=")] == ["CADDY_ADMIN=127.0.0.1:2019"], "CADDY_ADMIN is not exactly loopback-only")
require((config.get("Labels") or {}).get("azud.proxy.runtime") == revision, "runtime revision label is missing")
require(config.get("Entrypoint") in ("/bin/sh", ["/bin/sh"]), "entrypoint is not /bin/sh")
require(config.get("Cmd") == ["-eu", "-c", "if [ -s /config/caddy/azud.json ]; then exec caddy run --config /config/caddy/azud.json; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile; fi"], "startup command differs from Azud recovery selector")
require(config.get("StopTimeout") == 30, "stop timeout is not 30 seconds")

networks = network.get("Networks") or {}
require(host.get("NetworkMode") in (expected_network, "bridge"), f"network mode is not the disposable rootless bridge: {host.get('NetworkMode')!r}")
require(set(networks) == {expected_network} and isinstance(networks.get(expected_network), dict), f"actual network membership differs from the disposable bridge: {sorted(networks)!r}")
require(host.get("ReadonlyRootfs") is True, "root filesystem is writable")
expected_caps = {"NET_BIND_SERVICE"}
effective_caps = item.get("EffectiveCaps")
bounding_caps = item.get("BoundingCaps")
if effective_caps is not None:
    require(exact_cap_set(effective_caps, expected_caps), f"effective capabilities are not exactly NET_BIND_SERVICE: {effective_caps!r}")
if bounding_caps is not None:
    require(exact_cap_set(bounding_caps, expected_caps), f"bounding capabilities are not exactly NET_BIND_SERVICE: {bounding_caps!r}")
intent_is_exact = "ALL" in {cap(value) for value in (host.get("CapDrop") or [])} and exact_cap_set(host.get("CapAdd"), expected_caps)
runtime_proof_is_exact = effective_caps is not None and bounding_caps is not None and exact_cap_set(effective_caps, expected_caps) and exact_cap_set(bounding_caps, expected_caps)
require(intent_is_exact or runtime_proof_is_exact, "neither HostConfig capability intent nor live effective/bounding capabilities proves NET_BIND_SERVICE-only confinement")
require(any(str(value).lower() in ("no-new-privileges", "no-new-privileges=true") for value in (host.get("SecurityOpt") or [])), "no-new-privileges is missing")
require(host.get("PidsLimit") == 256, "pids limit is not 256")
require(host.get("Memory") == 536870912, "memory limit is not 512 MiB")
require(host.get("MemorySwap") == 536870912, "memory+swap limit is not 512 MiB")
require(host.get("NanoCpus") == 4000000000, "CPU limit is not 4 CPUs")
require(host.get("ShmSize") == 16777216, "shared-memory limit is not 16 MiB")
require(not host.get("ReadonlyTmpfs", False) and not host.get("ReadOnlyTmpfs", False), "automatic read-only tmpfs is enabled")
require(option_set((host.get("Tmpfs") or {}).get("/tmp")) >= {"rw", "noexec", "nosuid", "nodev", "size=16m", "mode=1777"}, "/tmp tmpfs options drifted")
run_options = option_set((host.get("Tmpfs") or {}).get("/run"))
require(run_options >= {"rw", "noexec", "nosuid", "nodev", "size=8m"} and ({"mode=0755", "mode=755"} & run_options), "/run tmpfs options drifted")

limits = host.get("Ulimits") or []
require(1 <= len(limits) <= 2, f"unexpected ulimit count: {limits!r}")
limits_by_name = {}
for limit in limits:
    name = str(limit.get("Name", "")).strip().upper().removeprefix("RLIMIT_")
    require(name in ("NOFILE", "NPROC") and name not in limits_by_name, f"unexpected or duplicate ulimit: {limit!r}")
    limits_by_name[name] = limit
nofile = limits_by_name.get("NOFILE") or {}
require(nofile.get("Soft") == 65536 and nofile.get("Hard") == 65536, f"exact requested nofile ulimit is missing: {limits!r}")
if "NPROC" in limits_by_name:
    nproc_limit = limits_by_name["NPROC"]
    require(isinstance(nproc_limit.get("Soft"), int) and isinstance(nproc_limit.get("Hard"), int) and 0 <= nproc_limit["Soft"] <= nproc_limit["Hard"], f"implicit NPROC ulimit is malformed: {nproc_limit!r}")
require((host.get("RestartPolicy") or {}).get("Name") in ("", "no"), "disposable container has an active restart policy")

bindings = host.get("PortBindings") or {}
active_bindings = {key: value for key, value in bindings.items() if value}
expected_bindings = {"80/tcp": "8080", "443/tcp": "8443"}
require(set(active_bindings) == set(expected_bindings), f"unexpected published ports: {sorted(active_bindings)}")
for key, expected_port in expected_bindings.items():
    values = active_bindings[key]
    require(len(values) == 1, f"{key} does not have exactly one binding")
    require(str(values[0].get("HostPort")) == expected_port, f"{key} does not map to {expected_port}")
    require(values[0].get("HostIp") in ("", "0.0.0.0", "::"), f"{key} has an unexpected host IP")
active_live_ports = {key: value for key, value in (network.get("Ports") or {}).items() if value}
require(set(active_live_ports) == set(expected_bindings), f"live network has unexpected published ports: {sorted(active_live_ports)}")
require("2019/tcp" not in active_live_ports, "live network publishes admin port 2019")

volume_mounts = {}
tmpfs_mounts = set()
for mount in item.get("Mounts") or []:
    if str(mount.get("Type", "")).lower() == "volume":
        volume_mounts[mount.get("Destination")] = (mount.get("Name"), mount.get("RW"))
    elif str(mount.get("Type", "")).lower() == "tmpfs":
        tmpfs_mounts.add(mount.get("Destination"))
    else:
        raise SystemExit(f"FAIL: unexpected mount: {mount}")
require(volume_mounts == {"/data": (data_volume, True), "/config": (config_volume, True)}, "named volume set drifted")
require(tmpfs_mounts in (set(), {"/tmp", "/run"}), "top-level tmpfs mount set is partial or unexpected")
require((item.get("State") or {}).get("Running") is True, "Caddy container is not running")
print("PASS: exact Podman inspect contract")
PY

echo "== live admin mutation and protected restart recovery =="
readonly live_json='{"admin":{"listen":"127.0.0.1:2019"},"apps":{"http":{"servers":{"probe":{"listen":[":80"],"routes":[{"handle":[{"handler":"static_response","status_code":200,"body":"azud-netcup-live-v2"}]}]}}}}}'
printf '%s' "$live_json" | p exec -i "$container" curl \
	--silent --show-error --fail-with-body --max-time 30 \
	--noproxy '*' --proto '=http' \
	--request POST \
	--header 'Content-Type: application/json' \
	--data-binary @- \
	--url "http://${admin_listen}/load" >/dev/null
test "$(curl --silent --show-error --fail-with-body --max-time 5 --noproxy '*' --proto '=http' --url 'http://127.0.0.1:8080/')" = 'azud-netcup-live-v2'

p restart -t 30 "$container" >/dev/null
if ! wait_for_admin; then
	echo "FAIL: Caddy admin API did not recover after restart" >&2
	p logs "$container" >&2 || true
	exit 1
fi
p stats --no-stream "$container"
p logs --tail 100 "$container"
test "$(curl --silent --show-error --fail-with-body --max-time 5 --noproxy '*' --proto '=http' --url 'http://127.0.0.1:8080/')" = 'azud-netcup-upstream'

p stats --no-stream "$upstream"
echo "PASS: pinned Caddy image, rootless bridge DNS/upstream routing, 8080/8443 with no host 2019, stdin admin mutation, protected restart recovery, cgroups, and confinement"
AZUD_NETCUP_PROBE
