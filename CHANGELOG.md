# Changelog

Azud follows [Semantic Versioning](https://semver.org/). Release entries are
generated from the changes accumulated under “Unreleased” and the GitHub
release notes for the corresponding signed tag.

## Unreleased

## 1.1.1 - 2026-08-08

- Fixed rolling deployments to reconcile Caddy host aliases added after the
  initial deployment without replacing existing upstreams, allowing Automatic
  HTTPS to provision certificates for new domains while preserving scaled and
  canary routes.
- Updated the Go build, test, release, and container toolchain to Go 1.26.5 and
  the CI and contributor lint tooling to `golangci-lint` v2.12.2.

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
