# AZUD

`DEPLOYMENT CONTROL / PODMAN / SSH`

Azud coordinates zero-downtime deployments of containerized applications to
one or more Podman hosts.

| Specification | Value |
|---|---|
| Interface | Go command-line application |
| Transport | SSH |
| Runtime | Podman |
| Ingress | Caddy |
| State | Declarative YAML plus a durable operation journal |
| Automation | GitHub Actions, GitLab CI, and non-interactive shells |

## Operating model

| Function | Implementation |
|---|---|
| Traffic changes | Blue-green and canary deployment procedures with health checks |
| Ingress and TLS | Caddy reverse proxy with automatic HTTPS through Let's Encrypt |
| Host topology | One or more hosts grouped by application role |
| Desired state | Version-controlled YAML configuration |
| Recovery state | Durable deployment and canary journal used for rollback and CI handoff |
| Operator output | Explicit status labels, terminal-aware color, and a stable plain mode |

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
- `docs/SECURITY.md` - Security and secrets guidance
- `docs/CLI_REFERENCE.md` - Full CLI and config reference
- `docs/ADVANCED_USAGE.md` - Recipes and advanced configs
- `docs/CI_CD.md` - CI/CD examples
- `docs/OUTPUT.md` - Terminal, pipe, and CI output contract
- `docs/DESIGN_PHILOSOPHY.md` - Why Azud exists
- `docs/FAQ.md` - Common questions

## Installation

```bash
curl -fsSL https://raw.githubusercontent.com/lemonity-org/azud/v1/scripts/install.sh | sh
```

The installer requires the [GitHub CLI](https://cli.github.com/) and verifies both the
release checksum and its GitHub/Sigstore build provenance. A source install with
`go install github.com/lemonity-org/azud@latest` is supported for development, but
cannot contain the release metadata injected into official binaries.

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
     ssl: false
     # ssl_redirect: true
     app_port: 3000
     # upstream_protocol: http # http, h2c, or https
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
       # readiness_cmd: "grpc_health_probe -addr 127.0.0.1:3000"
       # liveness_path: /live
       # disable_liveness: true
       # liveness_cmd: "curl -fsS http://localhost:3000/up"
       # helper_image: "docker.io/curlimages/curl:8.5.0@sha256:08e466006f0860e54fc299378de998935333e0e130a15f6f98482e9f8dab3058"
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

## Output

Azud renders a compact operations record. Written labels carry state; color is
secondary and is disabled automatically for pipes and CI.

```text
  # Deploy / ghcr.io/acme/api:v42
  --------------------------------------------------------
  INFO   Deploying to 2 hosts
  HOST   app-01 / Starting container
  OK     app-01 / Container started
  ERROR  app-02 / Readiness check failed
```

Use `azud version --short` in scripts. See
[`docs/OUTPUT.md`](docs/OUTPUT.md) for the complete output contract.

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
| `azud rollback <version>` | Rollback to a previous version |
| `azud history list` | Show recent deployment history |
| `azud setup` | Bootstrap servers and deploy |
| `azud preflight` | Validate hosts and configuration before deploying |

### Application
| Command | Description |
|---------|-------------|
| `azud app logs` | View application logs |
| `azud app exec -- <command>` | Execute command in container |
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
| `azud accessory boot <name>` | Start an accessory |
| `azud accessory stop <name>` | Stop an accessory |
| `azud accessory logs <name>` | View accessory logs |

### Cron Jobs
| Command | Description |
|---------|-------------|
| `azud cron boot` | Start scheduled tasks |
| `azud cron run <name>` | Run a job immediately |
| `azud cron list` | List all cron jobs |

### Canary
| Command | Description |
|---------|-------------|
| `azud canary deploy` | Deploy to small traffic % |
| `azud canary weight <percentage>` | Adjust traffic percentage |
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
  acme_email: ops@example.com
  # ssl_redirect: true
  app_port: 3000
  # upstream_protocol: http # http, h2c, or https
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

- The Go version declared in `go.mod`
- Podman on target servers
- SSH access to target servers

## Contributing and security

Contributions are welcome. Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before
opening a pull request and follow the [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md)
in all project spaces.

Please report vulnerabilities privately using the process in
[`docs/SECURITY.md`](docs/SECURITY.md), not through a public issue.

## License

MIT
