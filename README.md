<p align="center">
  <img src="azud-logo.png" alt="Azud Logo" width="200">
</p>

# Azud

**Deploy web apps anywhere with zero downtime**

Azud is a modern deployment tool for containerized web applications. It deploys Docker-based applications to any server with zero-downtime deployments, combining the simplicity of Dokku with the multi-server capabilities of Kamal—while addressing the pain points of both.

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

## Installation

```bash
go install github.com/adriancarayol/azud@latest
```

## Quick Start

1. **Initialize** your project:
   ```bash
   azud init
   ```

2. **Configure** your deployment in `config/deploy.yml`:
   ```yaml
   service: my-app
   image: ghcr.io/user/my-app

   servers:
     web:
       hosts:
         - 192.168.1.1

   proxy:
     host: example.com
     ssl: true
     app_port: 3000
     healthcheck:
       path: /up

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

## Commands

### Deployment
| Command | Description |
|---------|-------------|
| `azud deploy` | Deploy with zero-downtime |
| `azud redeploy` | Redeploy without rebuilding |
| `azud rollback [version]` | Rollback to a previous version |
| `azud setup` | Bootstrap servers and deploy |

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
  host: example.com
  ssl: true
  app_port: 3000
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
```

## Architecture

Azud is built in Go with a clean internal architecture:

- **CLI** - Cobra-based command interface
- **Config** - YAML configuration management with secrets handling
- **Docker** - Container lifecycle, registry, and health checks
- **Proxy** - Caddy management and route configuration
- **Deploy** - Blue-green deployment orchestration
- **SSH** - Remote server communication with bastion support

## Requirements

- Go 1.21+
- Docker on target servers
- SSH access to target servers

## License

MIT
