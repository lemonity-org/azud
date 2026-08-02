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
readonly container="${probe_id}"
readonly writer="${probe_id}-writer"
readonly network="${probe_id}-network"
readonly data_volume="${probe_id}-data"
readonly config_volume="${probe_id}-config"
inspect_file=""

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

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	set +e

	p rm -f "$writer" "$container" >/dev/null 2>&1
	[ -z "$inspect_file" ] || rm -f -- "$inspect_file"
	p volume rm "$data_volume" "$config_volume" >/dev/null 2>&1
	p network rm "$network" >/dev/null 2>&1

	cleanup_failed=0
	for name in "$writer" "$container"; do
		if p container exists "$name" >/dev/null 2>&1; then
			echo "FAIL: cleanup left container $name" >&2
			cleanup_failed=1
		fi
	done
	for name in "$data_volume" "$config_volume"; do
		if p volume exists "$name" >/dev/null 2>&1; then
			echo "FAIL: cleanup left volume $name" >&2
			cleanup_failed=1
		fi
	done
	if p network exists "$network" >/dev/null 2>&1; then
		echo "FAIL: cleanup left network $network" >&2
		cleanup_failed=1
	fi
	if [ "$cleanup_failed" -ne 0 ] && [ "$status" -eq 0 ]; then
		status=1
	fi
	if [ "$cleanup_failed" -eq 0 ]; then
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

echo "== exact image =="
p pull "$caddy_image"
p image inspect "$caddy_image" --format 'id={{.Id}} digest={{.Digest}} repo_digests={{json .RepoDigests}}'

p network create --label "io.azud.probe=${probe_id}" "$network" >/dev/null
p volume create --label "io.azud.probe=${probe_id}" "$data_volume" >/dev/null
p volume create --label "io.azud.probe=${probe_id}" "$config_volume" >/dev/null

readonly recovery_json='{"admin":{"listen":"127.0.0.1:2019"},"apps":{"http":{"servers":{"probe":{"listen":[":80"],"routes":[{"handle":[{"handler":"static_response","status_code":200,"body":"azud-netcup-probe\n"}]}]}}}}}'
printf '%s' "$recovery_json" | p run --rm -i \
	--name "$writer" \
	--label "io.azud.probe=${probe_id}" \
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
	-eu -c 'umask 077; mkdir -p /config/caddy; tmp="$(mktemp /config/caddy/.azud.json.XXXXXX)"; trap '\''rm -f "$tmp"'\'' EXIT HUP INT TERM; cat > "$tmp"; test -s "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" /config/caddy/azud.json; sync; trap - EXIT HUP INT TERM'

p run -d \
	--name "$container" \
	--label "io.azud.probe=${probe_id}" \
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
	-eu -c 'if [ -s /config/caddy/azud.json ]; then exec caddy run --config /config/caddy/azud.json; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile; fi' >/dev/null

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
	caps=$(awk "/^CapEff:/ {print \$2}" /proc/1/status)
	test $((0x$caps & 0x400)) -ne 0
	test $((0x$caps & ~0x400)) -eq 0
	awk "\$2 == \"/tmp\" && \$4 ~ /noexec/ && \$4 ~ /nosuid/ && \$4 ~ /nodev/ {ok=1} END {exit !ok}" /proc/mounts
	awk "\$2 == \"/run\" && \$4 ~ /noexec/ && \$4 ~ /nosuid/ && \$4 ~ /nodev/ {ok=1} END {exit !ok}" /proc/mounts
	test "$(cat /sys/fs/cgroup/memory.max)" = 536870912
	test "$(cat /sys/fs/cgroup/pids.max)" = 256
	test "$(cat /sys/fs/cgroup/cpu.max)" = "400000 100000"
	test "$(ulimit -n)" = 65536
	test "$(curl --silent --show-error --fail-with-body --noproxy \"*\" --proto \"=http\" --url http://127.0.0.1:2019/config/admin/listen)" = "\"127.0.0.1:2019\""
	test "$(curl --silent --show-error --fail-with-body --noproxy \"*\" --proto \"=http\" --url http://127.0.0.1/)" = azud-netcup-probe
'

test "$(curl --silent --show-error --fail-with-body --noproxy '*' --proto '=http' --url 'http://127.0.0.1:8080/')" = 'azud-netcup-probe'
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
    "Config": {key: config.get(key) for key in ("Image", "User", "Env", "Labels", "Cmd", "Entrypoint", "StopTimeout")},
    "HostConfig": {key: host.get(key) for key in (
        "NetworkMode", "ReadonlyRootfs", "CapAdd", "CapDrop", "SecurityOpt",
        "PidsLimit", "Memory", "MemorySwap", "NanoCpus", "ShmSize",
        "PortBindings", "Tmpfs", "ReadonlyTmpfs", "ReadOnlyTmpfs", "Ulimits",
        "RestartPolicy",
    )},
    "NetworkSettings": {"Ports": network.get("Ports")},
    "Mounts": item.get("Mounts"),
    "State": {key: (item.get("State") or {}).get(key) for key in ("Running", "Status", "ExitCode", "Error")},
}
print(json.dumps(evidence, indent=2, sort_keys=True))

def require(condition, message):
    if not condition:
        raise SystemExit("FAIL: " + message)

def cap(value):
    return str(value).strip().upper().removeprefix("CAP_")

def option_set(value):
    return {part.strip() for part in str(value or "").split(",") if part.strip()}

require(item.get("ImageName") == expected_image or config.get("Image") == expected_image, "container does not retain the exact pinned image reference")
require(config.get("User") == "1000:1000", "Config.User is not 1000:1000")
require([value for value in (config.get("Env") or []) if value.startswith("CADDY_ADMIN=")] == ["CADDY_ADMIN=127.0.0.1:2019"], "CADDY_ADMIN is not exactly loopback-only")
require((config.get("Labels") or {}).get("azud.proxy.runtime") == revision, "runtime revision label is missing")
require(config.get("Entrypoint") in ("/bin/sh", ["/bin/sh"]), "entrypoint is not /bin/sh")
require(config.get("Cmd") == ["-eu", "-c", "if [ -s /config/caddy/azud.json ]; then exec caddy run --config /config/caddy/azud.json; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile; fi"], "startup command differs from Azud recovery selector")
require(config.get("StopTimeout") == 30, "stop timeout is not 30 seconds")

require(host.get("NetworkMode") == expected_network, "network mode differs from the disposable rootless bridge")
require(host.get("ReadonlyRootfs") is True, "root filesystem is writable")
require({cap(value) for value in (host.get("CapDrop") or [])} == {"ALL"}, "CapDrop is not exactly ALL")
require({cap(value) for value in (host.get("CapAdd") or [])} == {"NET_BIND_SERVICE"}, "CapAdd is not exactly NET_BIND_SERVICE")
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
require(len(limits) == 1, "ulimit set is not singular")
limit = limits[0]
require(str(limit.get("Name", "")).upper().removeprefix("RLIMIT_") == "NOFILE" and limit.get("Soft") == 65536 and limit.get("Hard") == 65536, "nofile ulimit drifted")
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
test "$(curl --silent --show-error --fail-with-body --max-time 5 --noproxy '*' --proto '=http' --url 'http://127.0.0.1:8080/')" = 'azud-netcup-probe'

p stats --no-stream "$container"
p logs --tail 100 "$container"
echo "PASS: pinned Caddy image, rootless bridge 8080/8443 with no host 2019, stdin admin mutation, protected restart recovery, cgroups, and confinement"
AZUD_NETCUP_PROBE
