# Security Policy and Deployment Guide

## Supported versions

Security fixes are made for the latest stable `1.x` release. Older releases
may not receive patches; upgrade to the newest release before reporting a
problem that may already have been fixed.

## Reporting a vulnerability

Do not disclose suspected vulnerabilities in a public issue, discussion, or
pull request. Use GitHub's
[private vulnerability reporting](https://github.com/lemonity-org/azud/security/advisories/new)
to contact the maintainers.

Include, when possible:

- The affected Azud version, component, and platform
- Reproduction steps or a minimal proof of concept
- The security impact and realistic attack prerequisites
- Any suggested mitigation or patch
- Whether the issue is already public or has a disclosure deadline

The maintainers aim to acknowledge a report within three business days and
provide an initial assessment within seven business days. We will coordinate
validation, remediation, release, and credit with the reporter. Please allow a
reasonable remediation period before public disclosure.

Good-faith research that follows this policy, avoids privacy violations and
service disruption, and uses only systems and data you are authorized to test
will not lead the project to initiate legal action against the researcher.

The CLI, installer, official container image, and GitHub Action are in scope.
Deployment-specific misconfiguration is normally out of scope unless Azud
creates it contrary to its documented security controls.

## Deployment security

Practical security recommendations for Azud deployments.

## SSH Access

- Use dedicated deploy keys, not personal keys
- Restrict key access with server-side users and least privilege
- Consider `ssh-agent` or hardware-backed keys locally
- Use `azud ssh trust --template` to generate trusted fingerprints for config

## Secrets Handling

- Keep `.azud/secrets` out of git
- Builds never send `.azud/secrets` at any depth, the configured
  `secrets_path` when it is inside the build context, or leftover
  `.secrets-*.tmp` files of the secrets writer next to them; ignore-file
  negations cannot re-include them. Remote builds also exclude hard links to
  the secrets file. Local builds pass these rules to `podman build
  --ignorefile`, which matches paths only, so keep hard-linked or renamed
  copies of secrets out of the build context.
- Store secrets in your CI secret store and reconstruct at runtime
- Prefer environment variables or secret managers in production
- `azud env push` and `azud setup` deliver the runtime secrets file only to
  hosts that run application, accessory, or cron containers. The remote
  builder and SSH bastion never receive it unless they also run a workload.
  If you used an earlier version with a remote builder or bastion, delete any
  leftover `~/.azud/secrets` (or your `secrets_remote_path`) on those hosts.
- Keep registry and custom TLS secret references out of `env.secret`; Azud
  rejects cross-scope reuse. `azud env push` excludes those references from the
  remote runtime file and delivers TLS material directly to Caddy over SSH
  stdin. Create a distinct application credential when one is also required.
  Certificate and key references must be paired, present, non-empty, parseable,
  and matching; proxy startup fails rather than falling back when they are not.

### Local secrets file

- `azud init`, `env set`, `env delete`, `env pull`, and `env edit` (when it
  creates the file) write `.azud/secrets` (or `secrets_path`) through one
  writer. It writes a private temporary file in the same directory, fsyncs it,
  and renames it into place with mode `0600`. A file that was readable by group
  or others is repaired, with a warning.
- `.azud` is created with mode `0700`, and an existing `.azud` is tightened to
  `0700` on write. Missing parents of a custom `secrets_path` are created with
  `0700`; existing directories are left unchanged.
- Azud refuses to write through a symbolic link at the secrets path. If you
  keep secrets outside the project, set `secrets_path` to the real file.
- `env set` accepts keys matching `[A-Za-z_][A-Za-z0-9_]*` and single-line
  values of at most 60 KiB only (no CR, LF, or NUL). `azud env push` and
  `azud setup` reject such values from every secrets provider before
  contacting a host, because the remote runtime env file is line-based too.
  Encode multi-line application secrets, for example as base64. Secrets
  referenced by `proxy.ssl_certificate` or `proxy.ssl_private_key` are never
  written to that file, so a PEM certificate or key may span lines.
- `azud init` keeps an existing `.azud/secrets` and existing hook scripts.
- Loading a secrets file that group or others can read prints a warning with
  the `chmod 600` fix. `azud preflight` fails on such a file.
- Local `azud build` always excludes `.azud/secrets` at any depth and an
  in-context `secrets_path` from the build context, after your own ignore
  rules. `azud init` scaffolds a `.dockerignore` that also excludes `.azud/`
  and `.git/`.

## Configuration Trust

- Treat `config/deploy.yml`, destination files, hooks, and the environment
  Azud runs in as trusted input, like a Makefile. Loading the configuration
  can run `secrets_command` on your machine, so do not run Azud commands in a
  repository you do not trust. The configuration is validated before
  secrets load. `azud config`, `init`, `version`, `help`, and shell
  completion never run `secrets_command`; every other command does when
  `secrets_provider: command` is set.
- Environment references in the config are expanded only inside values: an
  environment value cannot add keys, list items, or comments, and multi-line
  values are rejected. See "Environment Variable References" in
  `docs/CONFIG_REFERENCE.md`.
- Fields that a shell runs (`secrets_command`, cron commands, `cmd` values
  that use shell syntax) still receive expanded values as shell code. If a
  variable can carry less-trusted text, such as CI metadata, write `"$$NAME"`
  so the shell reads the variable itself.

## Rootless Containers

Podman supports rootless mode. Use it where possible for a smaller blast radius.
Rootless mode cannot bind privileged proxy ports (`80`/`443`) directly. Use
unprivileged ports behind a load balancer/NAT, or set `proxy.rootful: true`
to run only the proxy as rootful Podman.

You can enforce security policies in `config/deploy.yml`:

```yaml
security:
  require_non_root_ssh: true
  require_rootless_podman: true
  require_known_hosts: true
  require_trusted_fingerprints: true
```

## Network Exposure

- Only expose ports 80/443 (or your LB ingress)
- Keep app ports internal; proxy forwards traffic
- Lock down SSH with firewall rules
- The Caddy admin API is never bound to TCP or published to the host. It
  listens on a unix socket in a tmpfs private to the `azud-proxy` container,
  so application, accessory and cron containers on the `azud` network cannot
  read the proxy configuration (including custom TLS keys) or replace it.
  Azud reaches the socket only through `podman exec` over SSH. See
  `docs/TROUBLESHOOTING.md` for the operator command.

## TLS / HTTPS

- Use Caddy automatic TLS for public domains
- For internal-only services, consider terminating TLS at a load balancer

## Untrusted Remote Output

- Container logs, exec output, and remote stderr are treated as untrusted.
  Azud neutralizes terminal control sequences in them before printing; see
  `docs/OUTPUT.md` (Remote output).
- `--raw` on log and exec commands disables this; use it only for output you
  trust or that goes to a file or another program.
- Interactive sessions (`azud app exec -i`/`-t`) are never filtered: they give
  the container full control of your terminal by design.

## Related docs

- `docs/TROUBLESHOOTING.md`
