<p align="center">
  <img src="azud-logo.png" alt="Azud Logo" width="200">
</p>

# Azud

**Deploy web apps anywhere with zero downtime**

Azud is a modern deployment tool for containerized web applications. It deploys Podman-based applications to any server with zero-downtime deployments, combining the simplicity of Dokku with the multi-server capabilities of Kamal—while addressing the pain points of both.

## Why Azud?

Existing deployment tools have fundamental design issues:

- **Kamal** locks you into kamal-proxy with limited features, has verbose output, complex environment management, and no built-in SSL
- **Dokku** is single-host only, has downtime during upgrades, relies heavily on plugins, and has a fragile app state

Azud solves these with:

- **Battle-tested Caddy** as the reverse proxy with automatic HTTPS via Let's Encrypt
- **Multi-server support** from day one
- **Zero-downtime deployments** by default using blue-green pattern
- **Configuration as code** with simple YAML files
- **Stateless CLI** with version-controlled configuration
- **Clean, structured output** that's easy to read and parse

## Features

- **Zero-downtime deployments** with blue-green pattern and health checks
- **Automatic HTTPS** via Let's Encrypt through Caddy
- **Multi-server scaling** with dynamic instance management
- **Canary deployments** for gradual rollouts
- **Native cron jobs** with job locking and timeouts
- **Accessory management** for databases, caches, and other services
- **Rollback support** to any previous version
- **CI/CD native** with GitHub Actions templates

## Documentation

- `docs/README.md` - Documentation index
- `docs/GETTING_STARTED.md` - First deploy walkthrough
- `docs/CONFIG_REFERENCE.md` - Configuration reference
- `docs/HOW_IT_WORKS.md` - Architecture and deployment model
- `docs/OPERATIONS.md` - Day-2 tasks (logs, scaling, canary, rollbacks)
- `docs/PRODUCTION_CHECKLIST.md` - Production readiness checklist
- `docs/TROUBLESHOOTING.md` - Common failures and fixes
- `docs/CHEATSHEET.md` - Command quick reference
- `docs/MIGRATION_KAMAL2.md` - Move from Kamal 2 to Azud
- `docs/MIGRATION_DOKKU.md` - Move from Dokku to Azud
- `docs/SECURITY.md` - Security and secrets guidance
- `docs/CLI_REFERENCE.md` - Full CLI and config reference
- `docs/ADVANCED_USAGE.md` - Recipes and advanced configs
- `docs/CI_CD.md` - CI/CD examples
- `docs/DESIGN_PHILOSOPHY.md` - Why Azud exists
- `docs/FAQ.md` - Common questions

## Installation

```bash
go install github.com/adriancarayol/azud@latest
```

## Prerequisites

- Linux servers (Ubuntu/Debian/CentOS/RHEL/etc.) with SSH access
- Ports 80/443 open to the internet for HTTPS (or your load balancer forwards to them)
- DNS pointing your domain(s) to the target servers or load balancer
- Podman installed (Azud can bootstrap during `azud setup`)

## Quick Start

1. **Initialize** your project:
   ```bash
   azud init
   ```
   This creates `.azud/secrets` for local secrets. Keep it out of version control.

2. **Configure** your deployment in `config/deploy.yml`:
   ```yaml
   service: my-app
   image: ghcr.io/user/my-app

   servers:
     web:
       hosts:
         - 192.168.1.1

  proxy:
    hosts:
      - example.com
      - www.example.com
    ssl: true
    # ssl_redirect: true
    app_port: 3000
    # response_timeout: 30s
    # forward_headers: true
    # buffering:
    #   requests: true
    #   responses: true
    #   max_request_body: 10485760
    #   memory: 1048576
    # logging:
    #   request_headers: [Authorization, Cookie] # redacted from logs
    #   response_headers: [Set-Cookie] # redacted from logs
    healthcheck:
      path: /up
      # readiness_path: /ready
      # liveness_path: /live
      # disable_liveness: true
      # liveness_cmd: "curl -fsS http://localhost:3000/up"
      # helper_image: "curlimages/curl:8.5.0"
      # helper_pull: "missing"

  env:
    clear:
      RAILS_ENV: production
    secret:
      - DATABASE_URL
   ```

3. **Setup** your servers:
   ```bash
   azud setup
   ```

4. **Deploy** your application:
   ```bash
   azud deploy
   ```

## Build Configuration

You can customize builds with the `builder` section:

```yaml
builder:
  # Local or remote build
  remote:
    host: 1.2.3.4
    arch: amd64

  # Build target/platform
  target: production
  arch: amd64
  platforms: ["linux/amd64", "linux/arm64"]
  multiarch: true

  # Build secrets (Podman format)
  secrets:
    - "id=npmrc,src=.npmrc"
    - "id=git_token,env=GIT_TOKEN"

  # SSH forwarding for builds
  ssh:
    - "default"

  # Build cache
  cache:
    type: registry
    options:
      ref: ghcr.io/user/my-app-cache
```

## Commands

### Deployment
| Command | Description |
|---------|-------------|
| `azud deploy` | Deploy with zero-downtime |
| `azud redeploy` | Redeploy without rebuilding |
| `azud rollback [version]` | Rollback to a previous version |
| `azud setup` | Bootstrap servers and deploy |
| `azud preflight` | Validate hosts and configuration before deploying |

### Application
| Command | Description |
|---------|-------------|
| `azud app logs` | View application logs |
| `azud app exec [cmd]` | Execute command in container |
| `azud app start/stop/restart` | Control application lifecycle |
| `azud app details` | Show container status |

### Scaling
| Command | Description |
|---------|-------------|
| `azud scale web=3` | Set absolute instance count |
| `azud scale web=+1` | Scale up by 1 |
| `azud scale web=-1` | Scale down by 1 |

### Proxy
| Command | Description |
|---------|-------------|
| `azud proxy boot` | Start the reverse proxy |
| `azud proxy stop` | Stop the proxy |
| `azud proxy status` | Show proxy and routes |

### SSH
| Command | Description |
|---------|-------------|
| `azud ssh trust [hosts...]` | Add host keys to known_hosts |

### systemd
| Command | Description |
|---------|-------------|
| `azud systemd enable` | Install and enable quadlet units |

### Accessories
| Command | Description |
|---------|-------------|
| `azud accessory boot [name]` | Start an accessory |
| `azud accessory stop [name]` | Stop an accessory |
| `azud accessory logs [name]` | View accessory logs |

### Cron Jobs
| Command | Description |
|---------|-------------|
| `azud cron boot` | Start scheduled tasks |
| `azud cron run [name]` | Run a job immediately |
| `azud cron list` | List all cron jobs |

### Canary
| Command | Description |
|---------|-------------|
| `azud canary deploy` | Deploy to small traffic % |
| `azud canary weight [%]` | Adjust traffic percentage |
| `azud canary promote` | Promote to full production |
| `azud canary rollback` | Rollback canary |

## Configuration

Azud uses a single YAML configuration file (`config/deploy.yml`):

```yaml
service: my-app
image: ghcr.io/user/my-app

registry:
  server: ghcr.io
  username: user
  password:
    - GITHUB_TOKEN

servers:
  web:
    hosts:
      - 192.168.1.1
      - 192.168.1.2
    options:
      memory: "512m"
      cpus: "0.5"
  worker:
    hosts:
      - 192.168.1.3
    cmd: bin/jobs

proxy:
  hosts:
    - example.com
    - www.example.com
  ssl: true
  # ssl_redirect: true
  app_port: 3000
  # response_timeout: 30s
  # forward_headers: true
  # buffering:
  #   requests: true
  #   responses: true
  #   max_request_body: 10485760
  #   memory: 1048576
  # logging:
  #   request_headers: [Authorization, Cookie] # redacted from logs
  #   response_headers: [Set-Cookie] # redacted from logs
  healthcheck:
    path: /up
    interval: 3s

env:
  clear:
    RAILS_ENV: production
    RAILS_LOG_TO_STDOUT: "true"
  secret:
    - DATABASE_URL
    - REDIS_URL
    - SECRET_KEY_BASE

accessories:
  mysql:
    image: mysql:8.0
    host: 192.168.1.10
    port: 3306
    volumes:
      - /var/lib/mysql:/var/lib/mysql
    env:
      clear:
        MYSQL_ROOT_PASSWORD: secret

cron:
  db_backup:
    schedule: "0 2 * * *"
    command: bin/rails db:backup
    lock: true

deploy:
  readiness_delay: 7s
  deploy_timeout: 30s
  drain_timeout: 30s
  retain_containers: 5

ssh:
  user: root
  keys:
    - ~/.ssh/id_rsa
  trusted_host_fingerprints:
    "192.168.1.1":
      - "SHA256:replace_with_fingerprint"

security:
  require_non_root_ssh: true
  require_rootless_podman: true
  require_known_hosts: true
  require_trusted_fingerprints: true

secrets_provider: file
secrets_remote_path: $HOME/.azud/secrets
```

Use `proxy.hosts` for multiple hostnames; `proxy.host` is still supported for single-host setups.

## Architecture

Azud is built in Go with a clean internal architecture:

- **CLI** - Cobra-based command interface
- **Config** - YAML configuration management with secrets handling
- **Podman** - Container lifecycle, registry, and health checks
- **Proxy** - Caddy management and route configuration
- **Deploy** - Blue-green deployment orchestration
- **SSH** - Remote server communication with bastion support

## Requirements

- Go 1.21+
- Podman on target servers
- SSH access to target servers

## License

MIT
