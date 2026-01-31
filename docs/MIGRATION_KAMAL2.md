# Migration from Kamal 2

This guide helps you move a typical Kamal 2 deployment to Azud.

## 1) Config mapping

| Kamal 2 | Azud |
|---------|------|
| `service` | `service` |
| `image` | `image` |
| `servers` | `servers` |
| `proxy` | `proxy` (Caddy) |
| `env` | `env` |
| `accessories` | `accessories` |
| `healthcheck` | `proxy.healthcheck` |

## 2) Create Azud config

```bash
azud init
```

Copy your existing values into `config/deploy.yml`. Pay attention to:

- `proxy.hosts` (domains)
- `proxy.app_port` (container port)
- `env.secret` entries (keys that live in `.azud/secrets`)

## 3) Registry auth

Set registry credentials in `config/deploy.yml`:

```yaml
registry:
  server: ghcr.io
  username: your-user
  password:
    - AZUD_REGISTRY_PASSWORD
```

Add the secret to `.azud/secrets`.

## 4) Secrets and env

Kamal uses env files and secrets. In Azud:

- Clear env vars go under `env.clear`
- Secret keys go under `env.secret`
- Secret values live in `.azud/secrets` (or another provider)

## 5) First deploy

```bash
azud setup
```

This boots Caddy, registers domains, and deploys your app.

## 6) Differences to keep in mind

- Azud manages Caddy instead of `kamal-proxy`
- Azud uses blue-green by default, with health checks
- CLI is stateless; config is the source of truth
- Use `azud build` + `azud deploy` instead of push-based deploys

## 7) CI/CD update

If you used Kamal’s CI flow, update your pipeline to:

```bash
azud build
azud deploy --skip-build
```

## Related docs

- `docs/GETTING_STARTED.md`
- `docs/CI_CD.md`
