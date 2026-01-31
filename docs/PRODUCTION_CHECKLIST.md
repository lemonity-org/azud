# Production Checklist

Use this before the first production launch and after major changes.

## Infrastructure

- DNS points to the correct servers or load balancer
- Ports 80/443 open to the world (or forwarded by LB)
- SSH access restricted (firewall, IP allowlist)
- Time sync enabled (NTP) on all servers

## Configuration

- `config/deploy.yml` committed and reviewed
- `config/deploy.production.yml` overrides validated (if used)
- `proxy.app_port` matches your app port
- `proxy.healthcheck.path` returns `200 OK`
- `deploy.readiness_delay` set if boot is slow

## Security

- `.azud/secrets` is git‑ignored
- Secrets exist for all `env.secret` keys
- `security.require_non_root_ssh` enabled
- `security.require_rootless_podman` enabled if supported
- `security.require_known_hosts` enabled and hosts trusted

## Build & Registry

- Image is pushed to the registry (`azud build`)
- Registry login works on all hosts (`azud registry login`)
- Tag strategy is stable per environment (consider `builder.tag_template`)

## Deploy & Rollback

- `azud preflight` passes
- First deploy done with `azud setup`
- Rollback validated (`azud rollback <version>`)

## Observability

- Log access verified (`azud app logs`)
- Proxy logs checked (`azud proxy logs`)
- Alerts configured for downtime or health failures

## Data & Backups

- Database backups tested (restore works)
- Persistent volumes validated
- Cron jobs for backups configured and tested

## Day‑2 Operations

- Scaling tested (`azud scale`)
- Canary flow tested (`azud canary deploy/promote`)
- Systemd units enabled if you want auto‑start (`azud systemd enable`)

## Go‑Live

- Deploy a known good version
- Confirm health checks
- Verify real traffic

## Related docs

- `docs/TROUBLESHOOTING.md`
- `docs/OPERATIONS.md`
