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

## TLS / HTTPS

- Use Caddy automatic TLS for public domains
- For internal-only services, consider terminating TLS at a load balancer

## Related docs

- `docs/TROUBLESHOOTING.md`
