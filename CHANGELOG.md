# Changelog

Azud follows [Semantic Versioning](https://semver.org/). Release entries are
generated from the changes accumulated under “Unreleased” and the GitHub
release notes for the corresponding signed tag.

## Unreleased

- Added `agent.deploy_command` and `agent.deploy_timeout`. The agent follows
  its tag and verifies each image as before, then hands the verified image to
  this command on the server instead of running Azud's deploy, for services
  whose releases need their own steps. The command runs under the host's
  deploy lock with `AZUD_AGENT_IMAGE` set, streams its output to the agent's
  log, and gets `SIGTERM` (then a kill a minute later) after the timeout
  (default 30m). It is not supported with `source: bundle`. See
  `docs/PULL_MODE.md`.

## 1.5.0 - 2026-10-31

- Added release bundles (#52). `azud release publish` pushes one OCI artifact
  per release with the image digest, the configuration files as written, and
  the runtime secrets encrypted with age to `release.recipients`;
  `azud release promote` moves a tag to it after signing, and
  `azud release inspect` shows what it contains without decrypting secrets.
- Added `agent.source: bundle`. The agent verifies a bundle, decrypts its
  secrets with the key from `azud agent init`, and deploys its image with its
  configuration, so changing secrets or configuration needs no SSH. The
  server's own configuration keeps every trust setting; bundles for another
  service, repository or host, and bundles older than the deployed one
  (unless `--allow-downgrade`), are refused; secrets are swapped under the
  deploy lock, and a failed deploy restores the previous secrets and
  redeploys the previous release; hooks never run for a bundle. What the
  agent does not apply (accessories, cron and Podman settings that differ
  from the server's configuration, accessories running with old secrets) is
  reported by `azud agent status` until it matches.
- Added the dependency `filippo.io/age` v1.3.2, the reference Go
  implementation of age, for the secrets in release bundles.

## 1.4.0 - 2026-10-31

- Added pull-mode deploys. `azud agent run` runs on the server, follows the
  registry tag in `agent.track`, and deploys each new digest it points to on
  that server, after `agent.verify_command` (for example `cosign verify`)
  passes. CI then needs no SSH access or deploy secrets. `azud agent status`
  shows what the agent saw and deployed, and `azud agent unit` prints a
  systemd user unit. Pull mode supports services that run on one server; see
  `docs/PULL_MODE.md`.
- Fixed rollback after a digest-pinned deploy. History records such a deploy's
  version as its digest, and automatic fleet rollback, `azud rollback`,
  `azud deploy --version`, `azud canary deploy --version` and
  `azud systemd enable` built the invalid reference `image:sha256:...` from
  it. They now use `image@sha256:...`.
- Updated Go to 1.26.10 for standard-library advisories GO-2026-6599,
  GO-2026-6600, GO-2026-6603, GO-2026-6604, GO-2026-6605, GO-2026-6607 to
  GO-2026-6613, GO-2026-6617, and GO-2026-6809. None was reachable from
  Azud's code, but the fixed packages are linked into the binary.

## 1.3.0 - 2026-10-28

- Fixed the vulnerabilities described in GHSA-ff4m-7w2r-4jf9, most
  importantly workload containers being able to read and replace the Caddy
  proxy configuration through its unauthenticated admin API. Read the
  **Breaking** entries below before upgrading: the first deploy or
  `azud proxy boot` recreates the proxy once, Quadlet-managed proxies need
  `azud systemd enable`, and cron jobs with `lock: true` need
  `azud cron boot`.
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
- **Breaking:** Changed `azud env push` and `azud setup` to deliver the
  runtime secrets file only to server, accessory, and cron hosts.
  `builder.remote.host` and `ssh.proxy.host` no longer receive application
  secrets unless they also run a workload, and `env push --host` and
  `env pull --host` reject them. Delete any leftover `~/.azud/secrets` (or
  `secrets_remote_path`) on builder-only and bastion-only hosts.
- **Breaking:** Changed remote build contexts to never include the
  configured `secrets_path` (matched by canonical path, including symlinked
  and hard-linked copies) or any nested `.azud/secrets`, even when
  `.containerignore`/`.dockerignore` negates them.
- Changed remote build contexts to extract into a private `mktemp -d`
  directory (`/tmp/azud-build-<service>.XXXXXXXXXX`, mode 0700) that is
  removed on every sync failure.
- **Breaking:** Changed `azud registry login/logout --host` to accept only
  configured server, accessory, cron, or remote builder hosts, rejecting
  others before the password is read. `registry.server` must now be
  `host[:port][/namespace]` (an optional `http(s)://` prefix is accepted),
  `registry.username` must not start with `-` or contain whitespace or
  control characters, and every `podman login`/`logout` passes `--` before
  the server.
- **Breaking:** Fixed `azud env pull` reporting "No secrets found" and
  exiting 0 when the remote secrets file or its directory could not be
  read. It now fails, never writes partial output locally, and still exits
  0 with a clear message when the file does not exist.
- **Breaking:** Changed `secrets_provider: env` to skip Azud's own
  `AZUD_*` variables (`AZUD_STATE_DIR`, hook variables, `AZUD_SSH_KEY`,
  installer and test variables) and variables named exactly like a
  registry password reference, with a warning naming them; a bare `AZUD_`
  prefix also warns. Use a dedicated prefix such as `AZUD_SECRET_`. Custom
  TLS references must name the secret without the prefix.
- Added one hardened local secrets writer used by `azud init`, `env set`,
  `env delete`, `env pull`, and `env edit`. It writes an fsynced 0600
  temporary file in the same directory and renames it into place, so it
  never writes through symlinks or hard links and repairs a group- or
  other-readable file, with a warning. `.azud` is created with mode 0700.
- **Breaking:** Changed `azud env set/delete/pull/edit` and `azud init` to
  refuse a secrets path that is a symbolic link or not a regular file; set
  `secrets_path` to the real file instead. A `secrets_path` containing `..`
  is rejected, even before the path is cleaned.
- **Breaking:** Changed `azud env set` to reject keys that do not match
  `[A-Za-z_][A-Za-z0-9_]*` and values containing CR, LF, or NUL or longer
  than 60 KiB, and `azud env push` and `azud setup` to reject such values
  from every secrets provider before contacting a host, since an embedded
  newline could add or override entries in the remote env file. Encode
  multi-line application secrets, for example as base64.
- Changed `azud env set KEY` to read the value from stdin (a prompt without
  echo on a terminal, or one line from a pipe). Passing the value as an
  argument still works but is deprecated and prints a warning.
- Fixed `azud env set` rewriting the secrets file with only the new key
  when the existing file could not be read, and `azud init` overwriting an
  existing `.azud/secrets` and hook scripts.
- **Breaking:** Changed `azud preflight` to fail when the file-provider
  secrets file is readable by group or others; run `chmod 600` on it. Other
  commands still only warn, now with the fix in the message, and no longer
  warn on Windows.
- Fixed `azud init` skipping the `.azud/secrets` `.gitignore` rule when the
  file only mentioned the path in a comment, a near-miss pattern, a
  directory-only rule, or a negation. Only exact active rules count, a
  symlinked `.gitignore` is refused, and init also ignores the writer's
  `.azud/.secrets-*.tmp` files.
- Changed local `azud build` to always pass `podman build --ignorefile`
  with the effective ignore file plus `.azud/secrets` at any depth, an
  in-context `secrets_path` (also when it or the context is reached through
  a symlink) and the writer's temporary files appended last, so local
  images can no longer contain local secrets. This needs Podman 4 or later.
  `azud init` now creates or extends `.dockerignore` with `.azud/`,
  `.git/`, and `.azud/secrets`, and leaves a symlinked container ignore
  file unchanged with a warning.
- **Breaking:** Changed `app logs`, `accessory logs`, `proxy logs` and
  `cron logs` (including `--follow`) to neutralize terminal control
  sequences in remote output: colors are removed, CR becomes LF, and other
  control bytes, invalid UTF-8 and bidi overrides are shown as visible
  `\xNN`/`\uNNNN` text. Pass the new `--raw` flag for byte-exact output.
- Changed non-interactive `app exec` and `accessory exec` to neutralize
  terminal control sequences when writing to a terminal; piped output stays
  byte-exact, `--raw` disables the filter, and `-i`/`-t` sessions are
  unchanged.
- Fixed Azud's own records, `server exec` output, tables and `history show`
  errors passing terminal escape sequences from remote output to the
  terminal.
- Hardened the HTTP health-check probe builders to validate the probe path
  and single-quote the URL, podman build commands to quote each build
  option, and Quadlet generation to sanitize every unit value so a value
  ending in a backslash can no longer swallow the next directive.
- **Breaking:** Changed the Caddy admin API to listen only on a unix socket
  (`/run/azud-admin/admin.sock`) inside a tmpfs private to the `azud-proxy`
  container, in every network mode and in the Quadlet unit, so workload
  containers and host users can no longer reach it. Port 2019 is no longer
  published or bound. Scripts that used `curl localhost:2019` must use
  `podman exec azud-proxy curl --unix-socket /run/azud-admin/admin.sock
  http://localhost/...` (with `sudo -n` for a rootful proxy managed by a
  non-root user).
- **Breaking:** Existing proxy containers are recreated once on the next
  `azud proxy boot`, `azud proxy reboot`, `azud deploy` or canary deploy,
  with a brief interruption. Routes are kept through Caddy's autosave. The
  old container is kept as `azud-proxy-legacy` until the replacement
  answers; if it does not, Azud restores and restarts the old container
  and fails with the reason. The migration holds the host's Caddy lock, so
  deploys of different services by one SSH user cannot interleave it, and
  the old container's autosave and running state are saved to
  `proxy-migration.json` (mode 0600) in the Azud state directory until it
  finishes, so a retry after an interrupted migration can still restore
  them. Quadlet-managed proxies are not recreated
  imperatively: rerun `azud systemd enable`, and until then deploys and
  `azud preflight` fail with that hint.
- **Breaking:** Changed Azud to strip every Caddy `admin` setting other
  than the socket `listen` (for example `remote`, `identity`,
  `config.load`) from configs it loads, restores, persists or resumes, with
  a warning, and to refuse admin requests to `/config/admin`.
- Changed `azud systemd enable` to stop the previous proxy unit or
  container, normalize Caddy's autosave (or seed it from Azud's protected
  copy) and start the regenerated unit, starting it again if preparation
  fails. The proxy unit no longer mounts the Azud state directory.
- Hardened the Caddy proxy container with a read-only root filesystem,
  `--cap-drop all --cap-add NET_BIND_SERVICE` and `no-new-privileges`; the
  image user and ports are unchanged.
- Changed `azud preflight` to no longer require `curl` on proxy hosts, and
  fixed `azud proxy status` reporting 0 routes when the admin API could not
  be read. A failed proxy start now reports the last admin probe error.
- To downgrade, remove an imperative proxy with `podman rm -f azud-proxy`
  and run the older `azud proxy boot`; for a Quadlet proxy, see
  docs/TROUBLESHOOTING.md.

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
