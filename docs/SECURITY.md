# Security Guide

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
