#!/bin/sh
# Runs the end-to-end suite from any machine with Docker: a privileged Ubuntu container
# boots systemd and hosts Podman, and the suite runs inside it in one of the three
# topologies Azud supports. In CI the runner itself is the server; see the e2e job in
# tests.yml.
#
#   root    everything runs as root
#   deploy  a rootless user runs the app and the proxy, on high ports
#   mixed   a rootless user runs the app, and a rootful proxy listens on 80 and 443
#
# Usage: scripts/e2e-in-container.sh [root|deploy|mixed] [flags, e.g. -test.run=TestFirstDeploy]
set -eu

mode=${1:-deploy}
[ $# -gt 0 ] && shift
case "$mode" in
root | deploy | mixed) ;;
*)
	echo "usage: $0 [root|deploy|mixed] [test flags]" >&2
	exit 2
	;;
esac

image=azud-e2e-host
name=azud-e2e-$mode
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*)
	echo "unsupported Docker architecture: $arch" >&2
	exit 1
	;;
esac

work=$(mktemp -d)
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -o "$work/azud" ./cmd/azud
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go test -c -tags e2e -o "$work/e2e.test" ./e2e
# mktemp makes the directory 0700, which the container's deploy user may not enter.
chmod -R a+rX "$work"

docker build -q -t "$image" - >/dev/null <<'DOCKERFILE'
FROM ubuntu:26.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    podman netavark aardvark-dns crun uidmap passt fuse-overlayfs iptables nftables \
    systemd systemd-sysv dbus dbus-user-session libpam-systemd sudo ca-certificates curl \
  && useradd --create-home --shell /bin/bash deploy \
  && printf 'deploy ALL=(root) NOPASSWD: /usr/bin/podman\n' > /etc/sudoers.d/azud-e2e \
  && mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/deploy \
  && mkdir -p /etc/containers \
  && printf 'unqualified-search-registries = ["docker.io"]\n' > /etc/containers/registries.conf
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
DOCKERFILE

docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" --privileged --cgroupns=private \
	--tmpfs /run --tmpfs /run/lock \
	-v /var/lib/containers -v /home/deploy -v /root/.local \
	-v "$work:/e2e:ro" "$image" >/dev/null

for _ in $(seq 1 60); do
	state=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
	case "$state" in running | degraded) break ;; esac
	sleep 1
done
docker exec "$name" chown -R deploy:deploy /home/deploy

if [ "$mode" = root ]; then
	docker exec -e AZUD_E2E_BINARY=/e2e/azud -e AZUD_E2E_REQUIRE_SYSTEMD=1 -w /e2e "$name" \
		/e2e/e2e.test -test.v -test.timeout=30m "$@"
else
	proxy_rootful=false
	[ "$mode" = mixed ] && proxy_rootful=true
	uid=$(docker exec "$name" id -u deploy)
	for _ in $(seq 1 30); do
		docker exec "$name" test -S "/run/user/$uid/bus" && break
		sleep 1
	done
	docker exec -u deploy -w /home/deploy \
		-e HOME=/home/deploy -e USER=deploy \
		-e XDG_RUNTIME_DIR="/run/user/$uid" \
		-e DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
		-e AZUD_E2E_BINARY=/e2e/azud -e AZUD_E2E_REQUIRE_SYSTEMD=1 \
		-e AZUD_E2E_PROXY_ROOTFUL="$proxy_rootful" \
		"$name" /e2e/e2e.test -test.v -test.timeout=30m "$@"
fi
