# Azud CLI security review — JIT findings

**Review date:** 2026-10-27  
**Revision:** `28c2f20` (`ci(release): Wait for required checks before publishing (#43)`)  
**Toolchain:** Go 1.27.1, module `go 1.26.6`  
**Method:** source review plus a build-tagged JIT suite that drives the real CLI
against an in-process fake SSH server. 40 tests and two fuzz targets; normal
`go test ./...` remains green. Evidence: [`security/jit/results.log`](jit/results.log).

## Summary

| ID | Severity | Finding | Proof |
|---|---|---|---|
| JIT-01 | **High** | Unauthenticated Caddy admin API is reachable by every workload container on the `azud` network; custom TLS keys can be read and proxy config replaced | `TestFinding_CaddyAdminReachableFromSharedNetwork`, `TestFinding_CaddyConfigExposesTLSPrivateKeyToAdminAPI` |
| JIT-02 | **High** | `cron.lock: true` mounts the whole Azud state directory (including `caddy-config.json` with TLS keys) read-write into a workload container | `TestFinding_CronLockExposesAzudStateAndCaddyConfig` |
| JIT-03 | **High/Medium** | Environment expansion runs on raw YAML before parsing; an influenced env value becomes YAML structure, can disable host-key verification, and can execute `secrets_command` | `TestFinding_ConfigEnvExpansionBecomesYAMLStructure`, `TestFinding_ConfigEnvExpansionOverridesSSHSecurity` |
| JIT-04 | **Medium** | `azud env set` preserves an existing 0644 secrets file and follows symlinks; `.azud` is created 0755 | `TestFinding_EnvSetPreservesInsecureSecretsMode`, `TestFinding_EnvSetFollowsSymlinkAndClobbersTarget`, `TestFinding_EnvSetAllowsNewlineInjection` |
| JIT-05 | **Medium** | A failing `secrets_provider: command` includes the command's output (often the secrets) in the error | `TestFinding_SecretsCommandOutputLeaksOnFailure` |
| JIT-06 | **Medium** | `azud proxy stop` is fail-open: exits 0 and prints "Proxy stopped" when every host fails | `TestFinding_ProxyStopReportsSuccessOnFailure` |
| JIT-07 | **Low/Medium** | `-d/--destination` is interpolated into a filename without validation and can load arbitrary YAML | `TestFinding_DestinationPathTraversal` |
| JIT-08 | **Medium** | `env push` sends application secrets to the remote builder and SSH bastion, not just workload hosts | `TestFinding_EnvPushSendsSecretsToRemoteBuilder` |
| JIT-09 | **Medium** | Only the literal `.azud/secrets` is excluded from remote build contexts; a custom `secrets_path` is uploaded to the remote builder | `TestFinding_BuildContextUploadsCustomSecretsPath` |
| JIT-10 | **Medium** | `registry login --host` accepts unconfigured hosts and sends the registry password there; `registry.server` is not validated and becomes a podman option | `TestFinding_RegistryLoginAcceptsUnconfiguredHost`, `TestFinding_RegistryServerFlagInjection` |
| JIT-11 | **Low** | Remote stdout is printed without stripping ANSI/OSC control sequences | `TestFinding_TerminalEscapeInjectionFromRemoteOutput` |
| JIT-12 | **Low** | `env pull` ignores a remote read failure and reports "No secrets found" | `TestFinding_EnvPullMasksRemoteReadFailure` |
| JIT-13 | **Low/Medium** | The documented broad `AZUD_` secrets prefix captures unrelated `AZUD_*` variables and pushes them to hosts | `TestFinding_EnvPrefixPushesUnrelatedSecrets` |
| JIT-14 | **Low** | Remote build contexts use a predictable, default-umask `/tmp/azud-build-*` directory | `TestFinding_BuildContextTempDirIsPredictable` |
| JIT-15 | **Low** | A world-readable secrets file is only warned about; preflight has no mode check | `TestFinding_WorldReadableSecretsFileOnlyWarns` |
| JIT-16 | **Low** | The Caddy proxy container lacks capability drop, `no-new-privileges`, read-only rootfs, and a non-root user | `TestFinding_ProxyContainerLacksHardeningFlags` |
| JIT-17 | **Low/Medium** | `azud init`'s `.gitignore` check is a substring test; a comment mentioning `.azud/secrets` suppresses the ignore rule, leaving the secrets file committable | `TestFinding_InitGitignoreSubstringCheck` |
| JIT-18 | **Low/Medium** | `init` does not create a container ignore file, and local builds send the raw context; `COPY . .` can bake `.azud/secrets` into an image | `TestFinding_InitDoesNotProtectBuildContext` |
| JIT-19 | **Low** | Access-log redaction is opt-in; enabling logging produces a plain JSON encoder that can record Authorization/Cookie headers | `TestFinding_AccessLogRedactionIsOptIn` |

Latent issues in exported helpers (not reachable through current validated
configuration) are listed in [Latent unsafe helpers](#latent-unsafe-helpers).

The review also confirmed several controls that are implemented well; see
[What held up](#what-held-up). The most important overall result is JIT-01:
the prior hardening record deliberately made the Caddy admin address
"container-reachable" (`docs/AZUD_1.0.0_RELEASE_HARDENING.md`, AZ-001). This
review treats application containers as untrusted and shows that this makes the
proxy control plane part of the blast radius of a single workload compromise.

---

## JIT-01 — Caddy admin API is a shared, unauthenticated control plane

**Severity:** High  
**Preconditions:** bridge network mode (the default when `proxy.rootful` is
not used for a rootless app), ability to run code in any container on `azud`.

**Evidence**

The CLI sends this container command (captured by
`TestFinding_CaddyAdminReachableFromSharedNetwork`):

```text
podman run -d --name azud-proxy \
  -e 'CADDY_ADMIN=0.0.0.0:2019' \
  -p 80:80 -p 443:443 -p 127.0.0.1:2019:2019 \
  -v caddy_data:/data -v caddy_config:/config \
  -l 'azud.managed=true' -l 'azud.type=proxy' \
  --network azud --restart unless-stopped \
  docker.io/library/caddy:2.11.4-alpine@sha256:... \
  caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume
```

Application containers use `Network: "azud"` (`internal/deploy/container.go:111`),
and accessories/cron containers do the same. Caddy therefore listens on
`0.0.0.0:2019` on an interface reachable by every peer. The host publication is
loopback-only, but that does not restrict the container network.

**Impact**

Caddy's admin endpoint has no authentication by design. Any compromised
workload (an application dependency, a database image, a cron job) can:

- `GET http://azud-proxy:2019/config/` — read the full live configuration.
  With custom TLS certificates, `applyProxySettingsFrom` embeds the private key
  in `tls.certificates.load_pem` (`internal/proxy/manager.go:917-925`), and the
  admin API returns it. The same config is also persisted in
  `caddy-config.json`.
- `POST http://azud-proxy:2019/load` — replace routing, TLS, upstreams, and
  logging; redirect another tenant's traffic to an attacker, capture
  credentials, or add a `file_server` route rooted at `/data` to expose ACME
  and custom certificate private keys from the `caddy_data` volume.
- Use the proxy as a network pivot into other workloads and host-loopback
  services reachable from the Caddy container.

The persistence and rollback machinery then treats the attacker's config as
authoritative, so a deploy or reboot can re-install it.

**Why the prior hardening is insufficient**

AZ-001 fixed host reachability by making the admin endpoint reachable from the
container interface. That solved the operational problem but did not isolate
the control plane from untrusted workloads. "Container-reachable" is not
"trusted"; the app is the most likely compromise point.

**Remediation**

Strongly prefer an admin transport that is not on the workload network:

1. **Unix socket (recommended):** set the Caddy admin listen to
   `unix//azud-state/caddy-admin.sock`, bind-mount a dedicated host directory
   into the proxy container, and make Azud's SSH-side `curl` use
   `--unix-socket /path/to/caddy-admin.sock`. Application containers must not
   mount that directory. Verify the socket mode/owner.
2. **Dedicated admin network:** attach the proxy to a second network with a
   static address and bind the admin listener only to that address; do not
   attach application containers to it. This is more fragile than a socket.
3. **Host network + loopback** for mixed/rootful deployments is already safer
   (`CADDY_ADMIN=127.0.0.1:2019`), but local host users can still reach it.
4. Add a live regression test that a container on `azud` cannot reach the admin
   port, and keep the fake-SSH assertion that the boot command has no
   `0.0.0.0:2019` in bridge mode.

The existing `scripts/caddy_admin_contract_test.sh` exercises the admin verbs
but does not test reachability from a peer container; extend it.

**Live confirmation**

On a disposable host with a working Podman:

```bash
podman network create azud-jit
podman run -d --name azud-proxy-jit --network azud-jit \
  -e CADDY_ADMIN=0.0.0.0:2019 -p 127.0.0.1:20190:2019 \
  -v azud-jit-caddy-config:/config \
  docker.io/library/caddy:2.11.4-alpine \
  caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume
podman run --rm --network azud-jit docker.io/library/alpine:3.23 \
  sh -c 'wget -qO- http://azud-proxy-jit:2019/config/ | head -c 200'
```

A JSON config body from the second command proves reachability. Do not run a
`/load` mutation on a production host. A script is provided at
[`security/poc/06-caddy-admin-exposure.sh`](poc/06-caddy-admin-exposure.sh).

---

## JIT-02 — `cron.lock` exposes the Azud state directory read-write

**Severity:** High when `cron.lock: true`; Medium otherwise.  
**Preconditions:** a configured cron job with `lock: true`, ability to run code
in that container, and the usual container→host UID mapping (container root in
rootless Podman maps to the deploying user).

**Evidence**

`buildCronContainerConfig` mounts the entire state directory:

```go
if cronConfig.Lock {
    stateDir := state.Dir(cfg.SSH.User)
    containerConfig.Volumes = append(containerConfig.Volumes,
        fmt.Sprintf("%s:/var/lib/azud:rw", stateDir))
}
```

See `internal/cli/cron.go:612-621`. The same directory contains the proxy's
persisted config, because `proxy.CaddyConfigFile(user)` is
`state.ConfigFile(user, "caddy-config.json")` (`internal/proxy/manager.go:63-65`)
and `state.ConfigFile` is `Dir(user)+"/"+name` (`internal/state/dir.go:98-101`).
`TestFinding_CronLockExposesAzudStateAndCaddyConfig` asserts the mount and the
path relationship.

**Impact**

- With custom TLS, the cron container can read
  `/var/lib/azud/caddy-config.json` (0600) because rootless container root maps
  to the host user. In rootful mode, a root-running workload reads it directly.
- The mount is read-write. A compromised cron container can replace
  `caddy-config.json`; the next proxy reboot without an autosave restores it
  via `restoreConfig` and loads attacker JSON.
- It can also churn Azud lock files, interfering with deploys, and create
  symlinks inside the state directory (for example the fixed
  `caddy-config.json.tmp` path used by `persistConfig`), enabling file-clobber
  races against host-side writes.

The lock feature only needs a lock namespace, not the whole state directory.

**Remediation**

- Create a dedicated `${stateDir}/locks` (0700) and mount only that directory
  (ideally read-write only for the lock files) at `/var/lib/azud/locks`.
- Move Caddy config and any other sensitive state out of the lock directory.
- Use `O_NOFOLLOW`/atomic writes on the host side for all fixed temp names.
- Add a test that the cron mount path cannot contain `caddy-config.json` or any
  proxy/TLS material.

---

## JIT-03 — Environment expansion before YAML parsing is config injection

**Severity:** High when an attacker can influence an expanded environment
variable (for example a PR title/branch interpolated into a CI env var);
Medium otherwise.  
**Preconditions:** `config/deploy.yml` contains `${VAR}` / `$VAR`, and the
attacker controls the value of `VAR` in the Azud process environment.

**Evidence**

`safeExpandEnv` calls `os.Expand` on the **raw file bytes** before
`yaml.Unmarshal` (`internal/config/loader.go:76-92`, `loadFile` at 94-128).
A newline in the environment value is therefore YAML syntax. The test sets:

```yaml
service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
```

and

```text
SERVICE_VALUE="shop\nsecrets_provider: command\nsecrets_command: \"touch /tmp/CANARY\""
```

`azud config` parses the injected top-level keys, loads secrets (before
validation), and executes the command. `TestFinding_ConfigEnvExpansionBecomesYAMLStructure`
observes the canary.

The same primitive can add an `ssh:` block and silently set
`insecure_ignore_host_key: true`
(`TestFinding_ConfigEnvExpansionOverridesSSHSecurity`), or override `image`,
`proxy`, `builder`, `hooks_path`, etc. when those keys are absent from the base
file. Duplicate keys are rejected, but absent keys are free real estate.

**Impact**

- Local command execution on the developer/CI runner via `secrets_command`
  under `azud config` (and every command that loads config).
- Disabling host-key verification or changing SSH hosts, proxy routing, image,
  or security policy.
- Deploying attacker-controlled images or sending secrets to attacker-chosen
  endpoints when combined with JIT-08/JIT-10.

`secrets_provider: command` is documented as executing a command, but the
surprising part is that a *value* of an environment variable controls the
document structure. A config file with `${BRANCH}` in a comment or scalar is
common in CI-generated configs.

**Remediation**

1. Parse the YAML document first. Walk scalar nodes only and expand environment
   references there; never expand mapping keys or raw bytes.
2. Reject `\n`, `\r`, NUL, and other control characters in expanded values
   (or encode them as YAML string escapes), and cap the expanded length.
3. Where possible, require `ALL_CAPS` names and keep the existing `$HOME`
   special case.
4. Add a regression test that an env value containing `\nssh:\n...` remains a
   single scalar and cannot introduce keys.
5. Document that `azud config` loads secrets and is not a safe read-only
   operation on an untrusted repository (or add `--no-secrets` for validation).

---

## JIT-04 — `env set` weakens/overwrites local secret files

**Severity:** Medium  
**Preconditions:** local write access to the project (including a cloned
repository containing a `.azud/secrets` symlink).

**Evidence**

`runEnvSet` calls `os.MkdirAll(dir, 0755)` and `os.WriteFile(secretsPath, ...,
0600)` (`internal/cli/env.go:445-463`). `os.WriteFile` does **not** change the
mode of an existing file and follows symlinks. Tests:

- `TestFinding_EnvSetPreservesInsecureSecretsMode` starts with a 0644 secrets
  file; after `azud env set NEW value`, it remains 0644 (world-readable).
- `TestFinding_EnvSetFollowsSymlinkAndClobbersTarget` makes `.azud/secrets` a
  symlink to a victim file; `env set PWNED yes` overwrites the victim.
- `TestFinding_EnvSetAllowsNewlineInjection` shows a newline in a value creates
  an additional secret entry.

`azud init` also creates `.azud` with 0755 (`internal/cli/init.go:68-72`), and
`runEnvEdit` uses 0755. By contrast, `env pull` correctly creates a 0700
directory and an 0600 temp file, then renames (`internal/cli/env.go:260-300`);
`TestControl_EnvPullWritesPrivateSecretsFile` confirms the good path.

`loadSecretsFromFile` only warns on group/other-readable files
(`internal/config/loader.go:203-206`), and preflight has no mode check
(JIT-15).

**Impact**

- Secrets remain readable by other local users and by processes that can read
  the workspace.
- A malicious repository can ship a symlink at `.azud/secrets` (or the
  configured `secrets_path`) and cause Azud to truncate/overwrite an arbitrary
  user-writable file when the developer runs a documented command. The content
  is limited to the Azud secrets format, but it is still arbitrary file
  destruction and can break `authorized_keys`, shell startup files, or project
  metadata.
- Newline values silently create extra variables, which can shadow a real
  secret during `env push`.

**Remediation**

Create one hardened writer used by `env set`, `env delete`, and `init`:

- `MkdirAll(dir, 0700)`; `Lstat` the final path and reject symlinks; open with
  `O_WRONLY|O_CREATE|O_TRUNC|O_NOFOLLOW`; `Chmod(0600)` on the open fd; write,
  fsync, close, then rename over the target (or write a temp and rename).
- Validate keys (`^[A-Za-z_][A-Za-z0-9_]*$`) and reject newlines/CR in keys and
  values; prefer `azud env set KEY` reading the value from stdin/TTY rather than
  argv (see latent L-05).
- On load, fail (or at least fail preflight) when the file is group/other
  readable; do not only warn.
- Make `.azud` 0700 at init; hooks can remain 0755 inside.

---

## JIT-05 — Failing secrets command leaks its output

**Severity:** Medium  
**Preconditions:** `secrets_provider: command` and a command that prints
secrets before failing.

**Evidence**

`loadSecretsFromCommand` does:

```go
out, err := cmd.CombinedOutput()
...
return fmt.Errorf("secrets_command failed: %w (%s)", err, strings.TrimSpace(string(out)))
```

(`internal/config/loader.go:294-300`). `TestFinding_SecretsCommandOutputLeaksOnFailure`
uses a command that prints `DB_PASS=hunter2` and exits 1; `azud config` prints
`(DB_PASS=hunter2)` in the error, which goes to CI logs.

**Impact**

Secret disclosure in terminal scrollback, CI logs, and error trackers. A
secrets command that partially succeeds (for example prints a Vault dump and
then fails on one key) is a realistic trigger.

**Remediation**

- Do not include command output in the returned error. Log only exit status,
  and optionally a redacted diagnostic length/hash.
- If diagnostics are needed, send stderr to a separately redacted debug channel
  (never stdout, which is the secret channel).
- Add a regression test that error text does not contain any value from the
  command's stdout.

---

## JIT-06 — `proxy stop` is fail-open

**Severity:** Medium (operational safety)  
**Evidence**

`runProxyStop` logs per-host errors and then unconditionally reports success:

```go
for _, host := range hosts {
    if err := manager.Stop(host); err != nil {
        log.HostError(host, "failed to stop proxy: %v", err)
        continue
    }
}
log.Success("Proxy stopped")
return nil
```

(`internal/cli/proxy.go:235-243`). `TestFinding_ProxyStopReportsSuccessOnFailure`
returns a simulated `podman stop` failure from the fake SSH server; the CLI
exits 0 and prints "Proxy stopped".

**Impact**

Automation and incident responders can believe the public proxy is stopped
when it is still serving (or vice versa, a stop script continues to the next
step and removes state/containers while the listener is live). It also
contradicts the fail-closed behavior added for preflight/setup/trust in the
prior hardening work.

**Remediation**

Aggregate errors exactly like `runProxyBoot`/`runProxyReboot`:

```go
var failures []string
for ... { if err != nil { failures = append(...); continue } }
if len(failures) > 0 { return fmt.Errorf("proxy stop failed on %d host(s): %s", ...) }
```

Only print the success line when all hosts succeeded. Add a fake-SSH regression
test.

---

## JIT-07 — `--destination` path traversal

**Severity:** Low/Medium  
**Preconditions:** an attacker can influence the `-d/--destination` value
(for example a CI variable) and a YAML file exists at a reachable path.

**Evidence**

`getDestinationPath` builds `base+"."+destination+ext` and `filepath.Join`s it
with the config directory (`internal/config/loader.go:155-160`). The destination
is not validated. `TestFinding_DestinationPathTraversal` runs:

```bash
azud -c config/deploy.yml -d '/../../victim' config
```

which resolves to `victim.yml` in the working directory and merges it; the
output shows `Service: attacker-owned`.

**Impact**

Loading an arbitrary YAML file as a destination config can override `image`,
`servers`, `proxy`, `security`, `secrets_path`, `hooks_path`, etc. With an
attacker-controlled `-d`, this is a config-injection primitive; with a typo it
can load an unrelated file and silently change deployment behavior. It does not
by itself leak file contents (YAML errors show paths/keys, not values).

**Remediation**

- Validate destination names: `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`; reject `/`,
  `\`, `.`, `..`, NUL.
- Resolve the candidate path and verify it is inside the config directory
  (`filepath.Rel` and reject `..` prefixes) before loading.
- Add the traversal case to the normal loader tests.

---

## JIT-08 — Secrets are pushed to the bastion and remote builder

**Severity:** Medium  
**Preconditions:** default `env push`; a configured `builder.remote.host` or
`ssh.proxy.host`.

**Evidence**

`runEnvPush` defaults to `cfg.GetAllSSHHosts()` (`internal/cli/env.go:157`), and
that set includes `builder.remote.host` and `ssh.proxy.host` in addition to
workload hosts (`internal/config/config.go:948-977`).
`TestFinding_EnvPushSendsSecretsToRemoteBuilder` configures a web host and a
remote builder, both pointing at the fake SSH server; both receive the
`DB_PASS` write. `setup` also calls `runEnvPush` (`internal/cli/setup.go:84-90`),
so first-time setup has the same scope.

**Impact**

Application runtime secrets are delivered to infrastructure hosts that do not
need them. A compromised or shared builder/bastion, or a builder operator, gets
every application secret. This violates least privilege and enlarges the blast
radius of the build path (which already has code-execution capability).

**Remediation**

- Add a workload-host set (`GetAllHosts` + accessory hosts + cron hosts) for
  secret delivery; exclude `builder.remote.host` and `ssh.proxy.host` by
  default.
- Provide an explicit `env push --include-infrastructure` (or `--host`) opt-in.
- Consider pushing only keys declared in `env.secret` (plus explicit
  `--all`); see JIT-13.

---

## JIT-09 — Custom `secrets_path` is uploaded to the remote builder

**Severity:** Medium  
**Preconditions:** `builder.remote.host` and a custom `secrets_path` with the
file provider.

**Evidence**

The mandatory context exclusion is only the literal `.azud/secrets`:

```go
return append(patterns, ignorePattern{pattern: ".azud/secrets"})
```

(`internal/cli/context_sync.go:222-226`). `cfg.SecretsPath` is not considered.
`TestFinding_BuildContextUploadsCustomSecretsPath` sets
`secrets_path: config/prod.secrets`, captures the tar.gz that Azud streams to
the remote builder, and finds `config/prod.secrets` in it (while `.azud/secrets`
is correctly excluded).

**Impact**

The custom secrets file is written to the remote builder's build context (and
the builder can read it; see JIT-14 for the directory mode). This is a secret
spill to the build host and any local users/processes there.

**Remediation**

- Resolve `cfg.SecretsPath` relative to the config/context and add it to the
  mandatory excludes, canonicalizing paths so `./config/prod.secrets` and
  `config/prod.secrets` both match.
- Consider excluding the whole `.azud` directory except hooks, plus common
  secret-like files (`.env*`) by default.
- Add a test that the configured secrets path never appears in the archive.

---

## JIT-10 — Registry login scope and argument injection

**Severity:** Medium (credential exposure/flag injection)  
**Preconditions:** an attacker can influence `--host` or `registry.server`, and
the SSH host acceptance conditions for the unconfigured host are met.

**Evidence**

`runRegistryLogin` accepts an arbitrary `--host` without checking
`cfg.GetAllSSHHosts()` (`internal/cli/registry.go:87-96`).
`TestFinding_RegistryLoginAcceptsUnconfiguredHost` configures `127.0.0.2` but
runs `registry login --host 127.0.0.1`; the fake server receives the login
command and the stdin password.

`registry.server` is not validated. `RegistryManager.Login` builds:

```go
podman login --username <quoted> --password-stdin <quoted server>
```

(`internal/podman/registry.go:55-56`). Quoting prevents shell injection, but
podman still parses an argument beginning with `-` as an option.
`TestFinding_RegistryServerFlagInjection` sets
`registry.server: "--tls-verify=false"` and captures:

```text
podman login --username deployer --password-stdin '--tls-verify=false'
```

**Impact**

- The registry password is sent to an unconfigured SSH host. If `--host` comes
  from a less-trusted input and the attacker can present a host key accepted
  by known_hosts (or `insecure_ignore_host_key` is set), credentials are
  exposed. The target host's Podman then stores the token.
- `--tls-verify=false` disables TLS verification for the registry login. With
  a network attacker, the username/password can be captured. Other podman
  flags could be injected similarly. This is config-driven unless
  `registry.server` is itself populated from an env expansion (JIT-03).

**Remediation**

- Validate `--host` against `cfg.GetAllSSHHosts()` for login/logout (mirror
  `env push`).
- Validate `registry.server` as `hostname[:port]` with the same host/IP
  validation used elsewhere; reject a leading `-`, whitespace, and shell
  metacharacters. Validate `registry.username` similarly.
- Prefer an explicit `--server` form if podman supports it, or pass a `--`
  terminator before positional arguments.
- Add tests for `--tls-verify=false`, `--help`, `-R`, and host-scope.

---

## JIT-11 — Terminal escape injection from remote output

**Severity:** Low  
**Evidence**

`runAppLogs`/`runAccessoryLogs` use `fmt.Print(result.Stdout)` (for example
`internal/cli/app.go:173`), `server exec` uses `output.Logger.Output`
(`internal/cli/server.go:133-135`), and `proxy logs`/`cron logs` do the same.
`writeOutput` preserves the bytes. `TestFinding_TerminalEscapeInjectionFromRemoteOutput`
returns an OSC 52 clipboard sequence plus `\x1b[2J` from the fake server and
observes them verbatim.

**Impact**

A compromised container (or a malicious app dependency) can control the
operator's terminal: write the clipboard (OSC 52), clear the screen, spoof
prompts/output, hide warnings, or exfiltrate via terminal emulator features.
CI logs can carry escape sequences into other tools.

**Remediation**

- Escape/strip C0 controls and ESC sequences when stdout is a TTY or CI; allow
  `--raw` for deliberate use.
- Use a dedicated terminal-safe writer for remote output; keep machine output
  (for scripts) opt-in.
- Add a test with OSC 52, CSI clear, and cursor movement.

---

## JIT-12 — `env pull` masks remote read failures

**Severity:** Low  
**Evidence**

`readCmd` is `cat <path> 2>/dev/null || echo ''` and the CLI never checks
`result.ExitCode` (`internal/cli/env.go:237-245`). A permission error or missing
file produces empty stdout and the message "No secrets found on server".
`TestFinding_EnvPullMasksRemoteReadFailure` returns exit 1 with
`cat: permission denied`; the CLI exits 0.

**Impact**

An operator may believe the host has no secrets and overwrite/local-reset the
local file, or miss a broken deployment prerequisite. It is a correctness
failure with security-adjacent consequences for recovery.

**Remediation**

- Remove `|| echo ''`; run `test -f` first, then `cat`; check the exit status
  and distinguish "missing" from "unreadable".
- Return a non-zero error for permission/SSH failures.

---

## JIT-13 — Broad `AZUD_` prefix pushes unrelated environment secrets

**Severity:** Low/Medium  
**Evidence**

`loadSecretsFromEnv` uses `strings.HasPrefix(key, prefix)` with no further
filter (`internal/config/loader.go:258-279`). The config reference example uses
`secrets_env_prefix: AZUD_` (`docs/CONFIG_REFERENCE.md:294-299`). `env push`
then sends every matched value except registry/TLS references.
`TestFinding_EnvPrefixPushesUnrelatedSecrets` sets `AZUD_SSH_KEY` and
`AZUD_APP_SECRET`; both are pushed, including the SSH key material.

**Impact**

Any `AZUD_*` variable in the process environment becomes an application
secret delivered to hosts (and, via JIT-08, to builders/bastions). This includes
tool configuration (`AZUD_STATE_DIR`), CI variables, and user secrets that
happen to share the prefix.

**Remediation**

- Use a dedicated prefix such as `AZUD_SECRET_` and update docs/scaffold.
- Prefer pushing only keys declared in `env.secret`; support an explicit
  `--all`/`--include-unreferenced` for current behavior.
- Reject reserved `AZUD_*` tool variables from the secret set and log a
  warning when the prefix is too broad.

---

## JIT-14 — Predictable, default-umask remote build directory

**Severity:** Low  
**Evidence**

`syncBuildContext` uses:

```go
remotePath := fmt.Sprintf("/tmp/azud-build-%s-%d", cfg.Service, time.Now().UnixNano())
mkdirCmd := fmt.Sprintf("mkdir -p %s", shell.Quote(remotePath))
```

(`internal/cli/context_sync.go:40-45`). There is no `chmod 700` and no `mktemp`.
`TestFinding_BuildContextTempDirIsPredictable` captures
`mkdir -p /tmp/azud-build-shop-1793122646596145760`. The directory is created
with the remote user's umask (typically 0755); other local users can list it and
read source files. A local attacker can pre-create a symlink at a guessed path
(a classic CWE-377 race), and cleanup is only scheduled after a successful
sync.

**Impact**

Source code and any sensitive context files are exposed to other users on a
shared builder; symlink races can redirect extraction. When JIT-09 applies, the
custom secrets file is in that context.

**Remediation**

- Create a private directory on the remote host with `mktemp -d` (or `install
  -d -m 700` with a random name), then `chmod 700`; verify it is not a symlink.
- Use a trap to remove it on every error path, not only after a successful
  archive extraction.
- Prefer a per-build directory under the remote user's home rather than `/tmp`.

---

## JIT-15 — World-readable secrets file is only a warning

**Severity:** Low  
**Evidence**

`loadSecretsFromFile` prints a warning for `perm&0077 != 0` and continues
(`internal/config/loader.go:203-206`). `TestFinding_WorldReadableSecretsFileOnlyWarns`
loads a 0644 file successfully. `azud preflight` checks secret references but
not local modes.

**Impact**

The tool advertises a 0600 expectation (and generated files are 0600) but does
not enforce it. Combined with JIT-04, a developer can end up with a
world-readable file while all commands report success.

**Remediation**

- Make preflight fail on group/other-readable secrets (or offer an explicit
  `--allow-insecure-secrets` escape).
- Have `env set`/`pull`/`init` repair modes and refuse symlinks.

---

## JIT-16 — Proxy container lacks hardening

**Severity:** Low (defense in depth)  
**Evidence**

`TestFinding_ProxyContainerLacksHardeningFlags` captures the proxy `podman run`
and finds no `--cap-drop`, `--security-opt`, `--read-only`, or `--user`. The
helper container, by contrast, uses `CapDrop: all` and `NoNewPrivileges`
(`internal/proxy/manager.go:299-313`). Caddy is the most exposed process in the
deployment and runs the upstream image's default user.

**Impact**

If Caddy itself is exploited (or its admin API is used as in JIT-01), the
container has the default capability set and a writable root filesystem,
increasing the impact and the chance of container escape. This is a
defense-in-depth gap, not a standalone compromise.

**Remediation**

- Run Caddy as a non-root user and listen on unprivileged ports (publish
  80/443 to 8080/8443), or drop all capabilities and add only
  `NET_BIND_SERVICE`.
- Add `--security-opt no-new-privileges`, a read-only rootfs with writable
  `/config` and `/data`, and a tmpfs for `/tmp`.
- Keep the admin socket isolated (JIT-01).

---

## JIT-17 — `init` can leave `.azud/secrets` unignored

**Severity:** Low/Medium (accidental secret commit)  
**Evidence**

`appendToGitignore` decides whether to append using:

```go
if strings.Contains(string(content), ".azud/secrets") {
    return nil
}
```

(`internal/cli/init.go:540-563`). A `.gitignore` comment, a path such as
`.azud/secrets-old`, or a documentation line containing that substring
suppresses the rule. `TestFinding_InitGitignoreSubstringCheck` starts with
`# .azud/secrets are documented elsewhere` and runs `azud init`; the resulting
`.gitignore` has no actual `.azud/secrets` line, so the generated secrets file
is eligible for commit.

**Impact**

The generated project handles real secrets (`env.secret` values, registry
credentials). A developer following the scaffold can commit them, and the
prior audit's AZ-002 fix is bypassed by an innocuous comment.

**Remediation**

- Match exact, non-comment lines (trim whitespace, ignore `#`), or use
  `git check-ignore .azud/secrets` when a repo is present.
- Append the rule when it is not an exact active pattern; also add
  `.azud/secrets.*` and `.azud/*.secrets` if appropriate.
- Refuse to follow a symlinked `.gitignore`.
- Add a test with comments, negations, and near-miss patterns.

---

## JIT-18 — Build contexts protect secrets remotely but not locally

**Severity:** Low/Medium (secret baked into a pushed image)  
**Evidence**

The remote build path appends a mandatory context exclude for the literal
`.azud/secrets` (`internal/cli/context_sync.go:207-226`, JIT-09). The local
build path passes the raw context directly to `podman build`:

```go
args = append(args, context)
buildCmd := exec.Command("podman", args...)
```

(`internal/cli/build.go:220-240`). `azud init` creates `.azud/secrets` but no
`.dockerignore`/`.containerignore` (`internal/cli/init.go:68-86`).
`TestFinding_InitDoesNotProtectBuildContext` confirms the generated project has
a secrets file and no container ignore file.

A common Dockerfile (`COPY . .`) plus no `.dockerignore` puts `.azud/secrets`
into the image. For a local build that image is then pushed by the same
command; for a remote build Azud excludes the literal default path, but not a
custom `secrets_path` (JIT-09).

**Impact**

Application secrets can be embedded in image layers and pushed to a registry,
where they are readable by anyone with registry access and can persist in
layer history even after later removal. This is a realistic onboarding
mistake: the scaffold succeeds and the user assumes Azud protected the file
because `.gitignore` mentions it.

**Remediation**

- Generate `.dockerignore`/`.containerignore` in `init` with at least
  `.azud/`, `.git/`, and the configured `secrets_path`; add near-miss entries.
- For local builds, merge the same mandatory excludes (using Podman's
  `--ignorefile` or a temporary filtered context) and/or refuse to build when
  the effective ignore file does not exclude the secrets path.
- Add preflight checks for both git and container ignores.
- Extend the remote exclusion to custom `secrets_path` (JIT-09).

---

## JIT-19 — Access-log credential redaction is opt-in

**Severity:** Low  
**Evidence**

`applyProxySettingsFrom` only builds a `filter` encoder when the operator sets
`proxy.logging.redact_request_headers` or `redact_response_headers`
(`internal/proxy/manager.go:866-901`). With `logging.enabled: true` and no
redaction list, it emits a plain JSON encoder.
`TestFinding_AccessLogRedactionIsOptIn` captures the Caddy `/load` body and
confirms there is no `"filter"` encoder.

Caddy's JSON access log includes request headers. Authorization bearer tokens,
session cookies, and proxy credentials are therefore recorded on stdout
(`docker logs`/`journalctl`) unless the operator knows to add a redact list.
The generated config template does not enable logging, so this is an
enablement footgun rather than a default leak.

**Impact**

Credential disclosure into logs that are often retained and shipped to a
central system, plus accidental exposure through support bundles or CI logs.

**Remediation**

- Seed `redact_request_headers` with `Authorization`, `Cookie`,
  `Proxy-Authorization`, and `Set-Cookie` (response) by default whenever
  logging is enabled; allow an explicit `redact: []` opt-out.
- Document the default redaction set next to `logging.enabled`.
- Add a rendered-config test that asserts the default filter deletes those
  headers.

---

## Latent unsafe helpers

These are not reachable through the currently validated CLI configuration, but
they are exported or shared helpers that will become exploitable if a future
field/caller passes less-validated data. They are worth fixing now.

### L-01 — Health-check URL builders use Go quoting, not shell quoting

`deploy.BuildHTTPCheckCommand`, `BuildHTTPCheckExecCandidates`, and
`BuildHTTPCheckHelperCommand` use `strconv.Quote(url)`
(`internal/deploy/healthcheck.go:29`, `88`, `117`). Go quoting escapes `"` and
`\` but does not escape `$` or backticks, and shell command substitution still
runs inside double quotes. `TestFinding_HealthcheckBuildersExecuteDollarSubstitution`
passes `path="/$(touch CANARY)"` and observes execution.

The CLI is currently safe because `healthPathRegex` rejects `$`, backtick, and
other metacharacters (`internal/config/validator.go:60-73`); the helper itself
relies on that upstream check. Use `shell.Quote(url)` (single quotes) or
validate/reject inside the helper, and add tests that call the helper directly.

### L-02 — `BuildConfig.Options` is appended unquoted

`BuildConfig.BuildCommand` and `ManifestBuildConfig.ManifestBuildCommands`
append `c.Options...` raw (`internal/podman/image.go:158`, `~239`). No current
config field populates `Options`, but `TestFinding_BuildConfigOptionsAreShellEvaluated`
shows `--annotation=$(touch CANARY)` executes. Quote each option or validate an
allowlist. (The container `ContainerConfig.Options` path is correctly quoted
and tested.)

### L-03 — Quadlet `Restart`/`WantedBy` and network/volume names are unsanitized

`GenerateContainerFile` sanitizes most fields but writes `Restart=` and
`WantedBy=` raw (`internal/quadlet/generator.go:126-139`), and
`GenerateNetworkFile`/`GenerateVolumeFile` write the name raw
(`internal/quadlet/generator.go:147-170`). Callers currently pass constants
("always", "default.target", "azud"), so this is latent.
`TestFinding_QuadletUnsanitizedFieldsInjectDirectives` demonstrates injected
`ExecStartPre=`/`DNS=` lines.
Sanitize all INI values and quote where systemd expects a word; add a fuzz test
(the container-field fuzzer already passes).

### L-04 — Image reference validation is permissive

`isValidImageRef` asserts "Remaining ref must be a valid name" but only checks
non-empty; it accepts `--privileged`, `foo;bar`, and other non-repository
values (`internal/config/validator.go:1273-1329`).
`TestFinding_ImageReferenceValidationIsPermissive` shows `image: --privileged`
passes and is displayed by `azud config`. Shell quoting prevents shell
injection, but the value can become a podman option or produce invalid units.
Use a real reference parser or an OCI name regex and reject a leading `-`.

### L-05 — Secrets on argv

`azud env set <key> <value>` and `azud env get` place secret values on the
command line (`internal/cli/env.go:90-100`, `399-413`), exposing them to shell
history and `ps`. The registry flows correctly use stdin; apply the same pattern
to `env set` (read from stdin/TTY) and consider masking `get`.

### L-06 — `azud config` executes configured commands

`runConfig` is presented as "show resolved configuration", but config loading
runs `secrets_provider: command` via `/bin/sh -c`
(`internal/config/loader.go:294`). This is by design for the provider, but it
makes a read-looking command execute repository code. Document it clearly and
consider a `--no-secrets` validation mode; this is the execution primitive used
in JIT-03.

---

## What held up

These controls were tested and passed; keep them covered:

- **Shell quoting (`internal/shell`)** preserves 37 payload classes through
  `/bin/sh`, including single/double quotes, backslashes, `$()`, backticks,
  newlines, carriage returns, `;`, `&&`, `||`, pipes, redirections, globs,
  braces, `!`, `#`, `~`, unicode, and a 4 KiB value. `QuoteRemotePath` expands
  only documented `$HOME`/`~` prefixes and quotes the suffix.
- **Podman command builders** for container run/create, exec, logs, and build
  keep adversarial values as single argv elements (the only exception is the
  latent `Options` field, L-02). Tested with `$(touch CANARY)`, quotes, and
  backticks.
- **Config hardening**: 1 MiB size limit, strict schema with unknown-key
  suggestions, duplicate-key rejection, 10k YAML depth limit, and validators
  for hosts, proxy hosts, health paths, remote secrets paths, names, ports,
  durations, capabilities, tmpfs, cron schedules, and version formats.
- **Quadlet container fields** strip newlines/control characters and escape
  quotes/percent in environment and label values; the container-file fuzzer
  found no section/directive injection.
- **Hooks** reject `../` traversal and skip symlinks using `O_NOFOLLOW` plus an
  opened-fd `Fstat`, with no TOCTOU window in the tested path.
- **`env pull`** writes a 0600 file into a 0700 directory via temp+rename.
- **Registry password transport** uses `--password-stdin` (no argv leak) in the
  normal flow; the remaining issue is the destination/flag validation.
- **SSH transport** defaults to known_hosts (not insecure), supports trusted
  fingerprints, and fails closed when `require_trusted_fingerprints` is set.
- **Prior audit fixes** for state file modes (Caddy 0700/0600), atomic secret
  push, digest verification, and fail-closed proxy boot are present; this
  review found new gaps around their trust boundaries.

---

## Scope, method, and limitations

- Reviewed the CLI, config loader/validator, SSH/podman command construction,
  secrets, proxy/Caddy lifecycle, cron, accessories, hooks, systemd/quadlet,
  build context sync, installer action, and generated docs/config.
- Built the real binary and drove it against an in-process fake SSH server that
  records commands and stdin. Normal `go test ./...` and `go test -race
  -tags security ./security/jit` pass.
- No live Podman/Caddy daemon was available in this sandbox (rootless user
  namespaces could not be created). JIT-01 is proven at the configuration level:
  the exact boot command has `CADDY_ADMIN=0.0.0.0:2019` and `--network azud`,
  and application containers join `azud`. Confirm peer reachability on a
  disposable host before treating it as fully live-proven.
- Config files and hooks are executable by design. Findings that require a
  malicious config (JIT-03, JIT-07, JIT-10, L-02, etc.) matter most when a
  piece of config or environment is supplied by a less-trusted party
  (PR metadata, shared CI variables, generated config, a shared builder).
- The suite intentionally asserts vulnerable behavior so it can stay green
  while documenting issues. When a finding is fixed, move it to `TestControl_*`
  and invert the assertion. See `security/jit/README.md`.

## Reproduction

```bash
# Full JIT suite (builds the CLI once, fake SSH only)
./security/jit/run.sh

# Include fuzzers
FUZZ=1 FUZZTIME=5s ./security/jit/run.sh

# Focused tests
go test -tags security -v ./security/jit -run 'TestFinding_Caddy|TestFinding_Cron'
```

Individual shell PoCs are in [`security/poc/`](poc/).
