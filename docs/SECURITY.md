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
- Store secrets in your CI secret store and reconstruct at runtime
- Prefer environment variables or secret managers in production

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

### Caddy control plane

Azud does not publish Caddy's admin port. In bridge mode the admin API binds to
`127.0.0.1:2019` inside `azud-proxy`, and the CLI executes the pinned image's
`curl` binary with `podman exec`. Request bodies travel over stdin rather than
shell arguments. Host `curl` is therefore not a proxy prerequisite and a host
listener on port 2019 indicates runtime drift in bridge deployments. Mixed
rootful/rootless mode uses host networking, where container loopback is also
host loopback; host firewall and process isolation remain part of that mode's
trust boundary.

The managed proxy runs as numeric UID/GID `1000:1000` with a read-only image,
`no-new-privileges`, all capabilities dropped except `NET_BIND_SERVICE`, and
bounded memory, CPU, PID, file-descriptor, shared-memory, `/tmp`, and `/run`
resources. Only the named `/data` and `/config` volumes remain writable. Azud
stores a mode-0600 recovery configuration in `/config/caddy/azud.json` and a
second protected host copy; it never mounts the host copy into the container.
The startup shell has an explicit entrypoint and reads only the Azud recovery
file (falling back to the stock Caddyfile on a genuinely empty first
deployment), never Caddy's mutable autosave.

`azud preflight`, `azud proxy status`, and `azud proxy reconcile --check`
report this runtime security state. `azud proxy boot` and reconciliation repair
recreate a running legacy proxy only after moving its live admin listener to
container loopback and successfully writing both recovery copies. A stopped
or unreachable insecure proxy is rejected so routes or TLS material are not
silently discarded.

Azud also prevents two reboot supervisors from racing for `azud-proxy`.
`azud systemd enable --no-start` first writes the Quadlet and reloads systemd,
then leaves the live proxy process untouched while changing its Podman restart
policy to `no`. The command verifies that handoff, so the installed Quadlet is
the only component authorized to recreate the proxy on the next boot. A
drifted, missing, or stopped proxy is rejected before this transition.

For rollback, stop the proxy unit, restore the accepted mode-0600 host snapshot
at `~/.local/share/azud/caddy-config.json` (or
`/var/lib/azud/caddy-config.json` for root), run
`azud proxy stage-recovery --host <host>`, and only then start the proxy unit.
The staging command validates JSON, forces the admin listener to container
loopback, and writes the named volume atomically through a disposable pinned
Caddy container with `--network none`, a read-only root filesystem, all
capabilities dropped, and no published ports. It never sends config JSON in
argv or logs and does not require the live admin API.

## TLS / HTTPS

- Use Caddy automatic TLS for public domains
- For internal-only services, consider terminating TLS at a load balancer

## Related docs

- `docs/TROUBLESHOOTING.md`
