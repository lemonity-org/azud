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

```yaml
hooks:
  pre_connect: .azud/hooks/pre-connect
  pre_build: .azud/hooks/pre-build
  pre_deploy: .azud/hooks/pre-deploy
  post_deploy: .azud/hooks/post-deploy
```

## Related docs

- `docs/GETTING_STARTED.md`
- `docs/CLI_REFERENCE.md`
