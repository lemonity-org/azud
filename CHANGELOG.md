# Changelog

Azud follows [Semantic Versioning](https://semver.org/). Release entries are
generated from the changes accumulated under “Unreleased” and the GitHub
release notes for the corresponding signed tag.

## Unreleased

- Changed the release workflow to wait for the required test and live
  integration checks on the tagged commit to finish instead of failing when a
  tag is pushed while they are still running.
- **Breaking:** Changed cron `lock: true` to mount only a read-only,
  per-job lock directory (`/var/lib/azud-locks/cron/<service>/<job>` for
  root, `~/.local/share/azud-locks/cron/<service>/<job>` otherwise) at
  `/run/azud-cron-lock` instead of the whole Azud state directory
  read-write, so a cron workload can no longer read the persisted Caddy
  config or custom TLS keys, hold deploy locks, or plant symlinks. Upgrade
  every machine and CI job that runs `azud cron run` first, then run
  `azud cron boot` to recreate running cron containers; older clients lock
  only the previous file. `cron boot` pulls and validates the image and
  prepares the lock before replacing a container, so a failed boot leaves
  the old job scheduled.
- Fixed cron `lock: true` jobs in images that run as a non-root user
  always printing "Skipped: lock held".
- **Breaking:** Fixed `azud proxy stop` printing "Proxy stopped" and
  exiting 0 when stopping failed on a host. It now fails with
  `proxy stop failed on N host(s)`, treats a host without a proxy container
  as stopped, and refuses a systemd-managed proxy, printing the
  `systemctl stop` command to use instead.
- Fixed `azud cron stop` and `azud accessory remove` not recognizing
  Podman's lowercase "no such container" error.
- Changed persisting the Caddy config to write through a random temporary
  file without following a symlink at the final path, and restoring it to
  refuse a symlinked file.
- Changed proxy reconciliation to always remove the server's
  `logs.should_log_credentials` setting, so Caddy's built-in redaction of
  Authorization, Cookie, Proxy-Authorization and Set-Cookie in access logs
  stays on. Documented that other credential headers need
  `redact_request_headers` and that query strings are logged in clear.
- **Breaking:** Changed config environment expansion to substitute
  `${VAR}`/`$VAR` only inside YAML values after parsing, so an environment
  value can no longer add keys, list items, or comments (for example
  `hosts: [${HOSTS}]` with `HOSTS="a, b"` is now one invalid host). Values
  containing newlines or control characters, or longer than 64 KiB, are
  rejected with an error that names the variable; references in mapping
  keys are rejected and references in comments are ignored. `$$` now writes
  a literal `$` (for example `robot$$ci`), and destination files follow
  the same rules.
- Changed configuration loading to validate the configuration before
  loading secrets, so an invalid configuration never runs
  `secrets_command`.
- Fixed a failing `secrets_command` printing its output, often the secrets
  themselves, in the error. Only stdout is parsed as `KEY=VALUE` secrets,
  stderr is passed through to the terminal, stdout is limited to 1 MiB, and
  a background process that keeps the output open no longer defeats the
  30s timeout.
- **Breaking:** Fixed `-d/--destination` accepting path separators and
  `..`, which could merge a YAML file outside the config directory.
  Destination names must start with a letter or digit and contain only
  letters, digits, dots, underscores, and hyphens (at most 63), and an
  override file that exists but cannot be read is now an error.
- **Breaking:** Changed image reference validation (`image`,
  `accessories.*.image`, `proxy.healthcheck.helper_image`) to follow the
  OCI reference grammar, so values such as `--privileged`, `foo;bar`,
  uppercase repository paths, or surrounding whitespace are rejected when
  the config loads.
- **Breaking:** Changed `azud config`, `azud completion <shell>` and shell
  tab completion to stop running `secrets_command`. `azud config` validates
  without loading secrets (use `azud env list` to check that secrets load),
  and completion no longer loads the configuration.

## 1.2.1 - 2026-09-28

- Fixed first deployments of a service whose image shares its name (for
  example `ghcr.io/org/app` deployed as service `app`) failing with
  "failed to preserve old container ... no such container": container lookups
  used a bare `podman inspect`, which also matches images, volumes and
  networks. All container lookups now use `podman container inspect`.
- Updated Go to 1.26.6 and `golang.org/x/crypto` to v0.56.0 for
  GO-2026-5972 (`encoding/asn1`) and GO-2026-6354 / GO-2026-6355 (SSH channel
  denial of service).

## 1.2.0 - 2026-08-09

- Added explicit Caddy request-header, HTTP/1 full-duplex, response-flush, and
  stream-close-delay controls, including rolling reconciliation of streaming
  options without replacing active upstreams.
- Made custom TLS fail closed when certificate references are incomplete,
  missing, empty, malformed, or do not match instead of silently clearing TLS
  state or falling back to automatic certificate behavior.
- Changed Caddy restarts to resume autosaved live JSON instead of briefly
  serving the stock file-server route before Azud restores persisted state.

## 1.1.1 - 2026-08-09

- Added safe leftmost wildcard proxy hosts, wildcard-aware DNS preflight, and
  Caddy route ownership checks that reject wildcard/exact host overlap across
  services.
- Kept registry and custom TLS credentials out of remote application env files,
  rejected cross-scope secret reuse, and delivered multiline custom certificates
  directly to Caddy.
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
