# Changelog

Azud follows [Semantic Versioning](https://semver.org/). Release entries are
generated from the changes accumulated under “Unreleased” and the GitHub
release notes for the corresponding signed tag.

## Unreleased

## 1.2.0 - 2026-08-02

- Isolated Caddy's admin API inside `azud-proxy`: it now binds only container
  loopback in bridge mode, port 2019 is never published, and every Azud admin
  request runs a fixed, argument-safe `curl` through `podman exec` with request
  bodies on stdin.
- Hardened both imperative and Quadlet proxy runtimes with a non-root user,
  read-only root filesystem, one required capability, no-new-privileges,
  bounded tmpfs/shm, and CPU, memory, swap, file-descriptor, and PID limits.
- Added proxy-runtime drift detection and fail-closed migration. Running legacy
  proxies are secured and snapshotted before recreation; stopped or
  unsnapshotable insecure proxies are rejected rather than replaced blindly.
- Added an Azud-owned mode-0600 recovery config in Caddy's persistent config
  volume, so API-managed routes survive process and host restarts without
  exposing the host's protected state file to the non-root container.
- Added `azud proxy stage-recovery`, a validation and atomic staging path from
  an operator-restored host snapshot into the protected boot volume before a
  proxy or Quadlet start.
- Made cold-start execution independent of image command metadata with an
  explicit shell entrypoint, so the hardened recovery selector is identical in
  imperative Podman and generated Quadlet deployments.
- Made `systemd enable --no-start` hand the live proxy's reboot authority to
  the installed Quadlet without interrupting traffic, and verify Podman's
  competing restart policy is disabled.
- Refused starts and no-downtime supervisor handoffs unless systemd reports the
  daemon-reloaded Quadlet service as loaded from generator output and connected
  to its requested boot target.
- Fixed manual rollback, automatic fleet rollback, canary deployment, and
  systemd unit reconstruction of digest-pinned deployments so history versions
  such as `sha256:...` use `repository@sha256:...` instead of the invalid
  `repository:sha256:...` tag form.
- Made runtime drift verification accept Podman 5.4's rootless inspect
  normalization only when the live image digest, custom-network membership,
  effective and bounding capabilities, and safe implicit process limit prove
  the same hardened runtime contract.
- Added a disposable Netcup compatibility probe that runs through the real
  non-root deploy identity and verifies the pinned Caddy image, rootless bridge
  DNS and routing, confinement, protected restart recovery, and exact cleanup.
- Added an audited operator transport for the Netcup probe, allowing a
  passwordless operator to enter the deploy account without changing which
  identity owns or executes the rootless Podman runtime.

## 1.1.0 - 2026-07-25

- Added typed per-role container hardening and a rollback-safe `stop_first`
  strategy for singleton non-web workloads, including interrupted-deploy
  reconciliation and scale protection.
- **Breaking:** `ssh.command_timeout` must now be at least five seconds longer
  than the largest effective container stop timeout, so an SSH command can
  never be cut off mid-stop. Configurations that set it explicitly below that
  floor (with the default 30s stop timeout, anything under `35s`) now fail
  validation; raise the value or lower `deploy.stop_timeout`. When
  `ssh.command_timeout` is unset, the default is raised automatically.
- **Breaking:** `deploy.stop_timeout` and `servers.<role>.runtime.stop_timeout`
  must be whole seconds and at least `1s`; Podman expresses stop timeouts in
  seconds, and sub-second values were silently truncated to zero before.
- Changed application containers to run with an explicit `--stop-timeout`
  (default 30s) instead of relying on Podman's 10s default, so a graceful
  shutdown gets the configured window on `azud app stop`, deploys, and reboots.
- Fixed a rolling deploy leaving the previous container running under the
  stable role name's network alias while the final upstream was registered.
  The proxy could dial it and then lose the connection when it was removed,
  which passive health checking escalated into a route-wide 503 for the whole
  30s `fail_duration`. The previous container is now stopped, gracefully and
  while it is already out of the route, before the final upstream is added.
- Added a bounded upstream retry window to generated Caddy routes, so a route
  that momentarily has no available upstream retries instead of answering 503
  on the first request.
- Added stable Caddy route ownership IDs and explicit proxy reconciliation.
- Added configurable HTTP, h2c, and HTTPS application upstream transports.
- Added command-based readiness probes for gRPC, TCP, and custom checks.
- Fixed proxy listener separation so `ssl_redirect: true` delegates port 80
  redirects to Caddy and HTTP-only services never serve plaintext on port 443.
- Fixed registry authentication state and lock files to follow the effective
  remote SSH user during login, setup, and deploy operations.
- Reworked terminal, pipe, installer, Make, and CI output into a compact
  technical record with semantic status colors, ASCII fallbacks, per-writer
  capability detection, and narrow-terminal table reflow.
- Added the stable, unstyled `azud version --short` automation surface and
  documented the output compatibility contract.
- Updated Go module dependencies to `github.com/mattn/go-isatty v0.0.24`,
  `github.com/spf13/pflag v1.0.10`, `golang.org/x/crypto v0.54.0`,
  `golang.org/x/term v0.45.0`, and `golang.org/x/sys v0.47.0`.

## 1.0.0 - 2026-07-20

- Hardened zero-downtime deploy, rollback, scale, canary, proxy, secrets, and
  SSH failure handling for the 1.0 release line.
- Added strict configuration validation, durable local state, role-aware
  workloads, reproducible Quadlet units, and release provenance.

Earlier `v0.0.x-dev` builds were development previews and did not carry a
stable compatibility promise.
