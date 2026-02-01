# Configuration Reference

This is a focused reference for `config/deploy.yml`. It highlights the most
commonly used fields; see `docs/CLI_REFERENCE.md` for command details.

## Service and Image

```yaml
service: my-app
image: ghcr.io/your-org/my-app
```

## Servers and Roles

```yaml
servers:
  web:
    hosts:
      - 203.0.113.10
    options:
      memory: "512m"
      cpus: "0.5"
  worker:
    hosts:
      - 203.0.113.11
    cmd: bundle exec sidekiq
```

Role options:

- `cmd`: override container command
- `options`: Podman options like `memory`, `cpus`
- `labels`, `tags`, `env`: role-level metadata

## Proxy and Health Checks

```yaml
proxy:
  hosts:
    - example.com
  ssl: true
  app_port: 3000
  healthcheck:
    path: /up
    readiness_path: /ready
    liveness_path: /live
    interval: 3s
```

Other useful fields:

- `ssl_redirect`, `acme_email`, `acme_staging`
- `http_port`, `https_port`
- `response_timeout`, `response_header_timeout`
- `buffering`, `forward_headers`
- `logging` (redaction and toggles)

## Registry

```yaml
registry:
  server: ghcr.io
  username: your-user
  password:
    - AZUD_REGISTRY_PASSWORD
```

## Environment Variables

```yaml
env:
  clear:
    RAILS_ENV: production
  secret:
    - DATABASE_URL
```

## Builder

```yaml
builder:
  remote:
    host: 203.0.113.12
    arch: amd64
  platforms: ["linux/amd64", "linux/arm64"]
  multiarch: true
  secrets:
    - "id=npmrc,src=.npmrc"
  cache:
    type: registry
    options:
      ref: ghcr.io/your-org/my-app-cache
```

## Deployment Settings

```yaml
deploy:
  pre_deploy_command: "./migrate up"
  readiness_delay: 10s
  deploy_timeout: 10m
  drain_timeout: 30s
  retain_containers: 3
  retain_history: 20
  rollback_on_failure: true
  canary:
    enabled: true
    initial_weight: 10
```

`pre_deploy_command` runs in a one-off `--rm` container from the **new image**
after the image is pulled but before app containers are started. It shares the
same network, environment variables, and secrets as the app. Runs on the first
host only. If the command exits non-zero, the deploy aborts.

## Accessories

```yaml
accessories:
  postgres:
    image: postgres:15
    host: 203.0.113.10
    port: 5432
    env:
      clear:
        POSTGRES_DB: app_prod
      secret:
        - POSTGRES_PASSWORD
```

## Cron Jobs

```yaml
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
```

## SSH and Security

```yaml
ssh:
  user: ubuntu
  port: 22
  keys: ["~/.ssh/id_ed25519"]
  known_hosts_file: "~/.ssh/known_hosts"
  connect_timeout: 10s
  insecure_ignore_host_key: false
  trusted_host_fingerprints:
    "203.0.113.10":
      - "SHA256:..."
  proxy:
    host: bastion.example.com
    user: admin

security:
  require_non_root_ssh: true
  require_rootless_podman: true
  require_known_hosts: true
  require_trusted_fingerprints: true
```

## Secrets Providers

```yaml
secrets_provider: file   # file, env, command
secrets_path: .azud/secrets
secrets_env_prefix: AZUD_
secrets_command: ./bin/print-secrets
secrets_remote_path: "~/.azud/secrets"
```

## Volumes

```yaml
volumes:
  - /var/lib/my-app/uploads:/app/public/uploads
```

## Hooks

Hooks are executable scripts discovered by filename in the `hooks_path`
directory (default `.azud/hooks/`). There is no per-hook path configuration;
just place a file named after the hook in the directory and make it executable.

```yaml
hooks:
  timeout: 5m   # Maximum time a hook may run (default: 5m)
```

### Standard hooks

| Hook | Fires | Abort on failure |
|---|---|---|
| `pre-connect` | Before SSH connections are opened | Yes |
| `pre-build` | Before building the container image | Yes |
| `post-build` | After a successful local build, before push | No (warn) |
| `pre-deploy` | Before deploying to servers | Yes |
| `pre-app-boot` | Before starting a new container on each host | Yes |
| `post-app-boot` | After health check passes on each host | No (warn) |
| `post-deploy` | After all hosts are deployed | No (warn) |
| `pre-proxy-reboot` | Before booting/rebooting the proxy | Yes |
| `post-proxy-reboot` | After proxy boot/reboot completes | No (warn) |
| `post-rollback` | After automatic rollback completes | No (warn) |

Custom hooks (any non-standard filename in the hooks directory) can be run
manually with `azud hooks run <name>`.

### Environment variables

Every hook receives `AZUD_*` environment variables with deployment context.
Empty fields are omitted.

| Variable | Description |
|---|---|
| `AZUD_SERVICE` | Service name |
| `AZUD_IMAGE` | Full image reference |
| `AZUD_VERSION` | Image version/tag |
| `AZUD_HOSTS` | Target hosts (comma-separated) |
| `AZUD_DESTINATION` | Deployment destination |
| `AZUD_PERFORMER` | User running the command |
| `AZUD_ROLE` | Server role (when applicable) |
| `AZUD_HOOK` | Name of the executing hook |
| `AZUD_RECORDED_AT` | Timestamp (RFC 3339) |
| `AZUD_RUNTIME` | Deployment duration in seconds (post-deploy only) |

### CLI commands

```
azud hooks list          Show all hooks with status (ready/missing/not executable)
azud hooks run <name>    Run a hook with test AZUD_* context
```

## Related docs

- `docs/GETTING_STARTED.md`
- `docs/CLI_REFERENCE.md`
