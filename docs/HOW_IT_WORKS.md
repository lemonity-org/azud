# How Azud Works

This document explains the core pieces of Azud so you can reason about behavior in production.

## Deployment Model (Blue-Green)

Azud uses a blue-green deployment strategy:

1. Pulls the new image on each server (unless skipped)
2. Starts a new container set alongside the old one
3. Waits for health checks to pass
4. Updates the proxy to route traffic to the new containers
5. Drains and removes the old containers

This gives zero-downtime deploys for healthy apps and safe rollbacks.

## Health Checks

Azud waits for your app to pass health checks before switching traffic.
Configure them under `proxy.healthcheck` (path, interval) and adjust readiness
settings under `deploy` if your app needs extra warmup time.

## Proxy and HTTPS

Azud manages Caddy as the reverse proxy by default:

- Automatic HTTPS via Let's Encrypt
- Routes traffic to the correct container port
- Health checks gate traffic switching

You can disable managed proxy for non-HTTP workloads, or use your own load balancer in front.

## Images and Versions

`azud build` creates a version tag and (optionally) a `latest` tag. Deploys use:

- A specific tag via `azud deploy --version`
- Or the latest tag by default

Each destination can have its own image tag to avoid cross-environment conflicts.

## Build, Push, Pull

- `azud build` builds and pushes the image to your registry
- `azud deploy` pulls the image on servers and rolls it out
- `azud deploy --skip-build` deploys an existing registry image

This keeps CI/CD fast and avoids pushing large images from a laptop.

## Stateless CLI

Azud does not store server state locally. The CLI reads configuration from
`config/deploy.yml` and computes desired state. This keeps deploys reproducible
and version-controlled.

## Canary Deployments

Canaries split traffic between a stable version and a new version. Azud can
deploy a canary, adjust traffic weight, and promote or roll back:

```bash
azud canary deploy --version v1.2.3 --weight 10
azud canary weight 50
azud canary promote
```

## Roles and Hosts

Servers are grouped by role (`web`, `worker`, etc.). Deploys can target:

- All roles (default)
- One role (`--role`)
- One host (`--host`)

This makes it easy to isolate background jobs or deploy in stages.

## Related docs

- `docs/OPERATIONS.md`
- `docs/CLI_REFERENCE.md`
