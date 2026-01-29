# Azud Design Philosophy

Azud was created to address fundamental design flaws in existing deployment tools. This document outlines the problems we're solving and our approach.

---

## Kamal 2.0 Design Flaws

### 1. Kamal-Proxy Lock-in & Feature Regression

Kamal 2 replaced Traefik with their own kamal-proxy. This caused:
- Loss of middleware features (compression, rate-limiting, circuit breakers)
- [AWS ALB compatibility issues](https://blog.driftingruby.com/kamal-2-0-issue-with-aws-alb/) causing "Unsupported HTTP Method: PRI" errors
- No opt-out for non-HTTP services - proxy is mandatory even when unnecessary

**Azud's approach:** Use battle-tested Caddy as the default proxy, with the option to disable it entirely for non-HTTP workloads. Caddy provides compression, rate-limiting, and proper HTTP/2 support out of the box.

### 2. Container/Image Conflicts for Multi-Environment

[GitHub Issue #1184](https://github.com/basecamp/kamal/issues/1184) - deploying overwrites the registry image with different port configurations, breaking container restarts.

**Azud's approach:** Environment-specific image tags and container configurations that don't overwrite each other. Each destination maintains its own state.

### 3. Push-Based Architecture Problems

- Deploys from local machine require uploading entire Docker images
- Asymmetric ISP connections make first deploys extremely slow
- No native CI/CD-first design - requires workarounds for GitHub Actions

**Azud's approach:** Support both push and pull-based deployments. CI/CD-first design with native GitHub Actions support. Images are pulled from registries, not pushed from local machines.

### 4. Verbose, Unparseable Output

[Medium: Why I Gave Up on Kamal](https://medium.com/@daveydave/why-i-gave-up-on-kamal-eb68d1394fe6) - making debugging nearly impossible compared to Dokku's clear output.

**Azud's approach:** Clean, structured output with clear success/failure indicators. Verbose mode available when needed, but default output is human-readable and parseable.

### 5. Environment Variable Management

Requires chaining secrets across multiple tools and accounts. No direct server access - must manage through configuration files.

**Azud's approach:** Simple secrets file with direct server sync. Support for environment variables from multiple sources (files, environment, vault integrations).

### 6. No SSL/Single-Server Support

Kamal assumes you're behind a load balancer. No built-in Let's Encrypt, no single-server friendly features.

**Azud's approach:** Built-in Let's Encrypt via Caddy. Single-server deployments are first-class citizens, not an afterthought.

### 7. Cron Jobs Require Hacks

[Scheduling Cron Jobs with Kamal](https://glaucocustodio.github.io/2024/01/23/migrating-from-dokku-to-kamal-scheduling-cron-jobs/) requires significant extra work compared to Dokku.

**Azud's approach:** Native cron job support in configuration. Define schedules alongside your service definitions.

### 8. Version Compatibility Fragility

[GitHub Issue #1298](https://github.com/basecamp/kamal/issues/1298) - tight coupling between CLI and proxy versions.

**Azud's approach:** Loose coupling between components. CLI and proxy versions can evolve independently with clear compatibility matrices.

---

## Dokku Design Flaws

### 1. Single-Host Only Architecture

[Dokku vs Kamal comparison](https://deploymentfromscratch.com/tools/dokku-vs-kamal/) - no native multi-server support. Scaling horizontally requires external orchestration or plugins with "rough edges."

**Azud's approach:** Multi-server support from day one. Deploy to multiple hosts with a single command, with role-based server grouping.

### 2. Single-Tenant Security Model

All users have access to all functionality. Community auth plugins exist but each has limitations due to Dokku's core interfaces.

**Azud's approach:** SSH-based access control. Server access determines deployment permissions. No additional auth layer needed.

### 3. Downtime During Upgrades

A maintainer stated: [GitHub Issue #2131](https://github.com/dokku/dokku/issues/2131)

**Azud's approach:** Zero-downtime deployments by default. New containers are health-checked before old ones are removed.

### 4. Plugin-Heavy Architecture

Databases, schedulers, and advanced features all require plugins:
- Each plugin has its own config paradigm
- Plugin quality varies
- State management between core and plugins can conflict

**Azud's approach:** Core features built-in (accessories for databases, health checks, proxy management). Consistent configuration across all features.

### 5. App State Fragility

GitHub issues show recurring problems:
- Containers disappearing after running normally
- Domain settings clearing when renaming apps
- Deploy-branch getting incorrectly set during git sync

**Azud's approach:** Stateless CLI design. All configuration lives in version-controlled YAML files. Server state is derived from configuration, not stored separately.

### 6. No Process Scaling Without Restart

Can't scale processes dynamically - requires app restart.

**Azud's approach:** Dynamic scaling support. Add or remove container instances without affecting running services.

### 7. Bash Script Origins

Started as a 100-line bash script. While improved, some architectural decisions are constrained by this history.

**Azud's approach:** Purpose-built in Go with clean architecture. No legacy constraints.

### 8. Nginx Config Validation

No pre-validation of nginx configurations before deployment - failures happen at deploy time.

**Azud's approach:** Use Caddy with JSON API for configuration. Changes are validated before being applied.

---

## Common Flaws in Both

| Flaw | Kamal | Dokku | Azud Solution |
|------|-------|-------|---------------|
| Rollback is unintuitive | ✓ | ✓ | Simple `azud rollback <version>` with version history |
| Health check configuration complexity | ✓ | ✓ | Sensible defaults with simple override options |
| Registry/image cleanup issues | ✓ | ✓ | Automatic cleanup with configurable retention |
| Learning curve despite "simple" branding | ✓ | ✓ | Minimal configuration for simple cases, power when needed |
| Production readiness concerns | ✓ | ✓ | Battle-tested components (Caddy, Docker), comprehensive testing |

---

## Azud Design Principles

1. **Configuration as Code** - Everything in version-controlled YAML
2. **Sensible Defaults** - Works out of the box, customize when needed
3. **Multi-Server First** - Scaling is not an afterthought
4. **Zero-Downtime Always** - Blue-green deployments by default
5. **CI/CD Native** - Designed for automation, not just local use
6. **Battle-Tested Components** - Use proven tools (Caddy, Docker), don't reinvent
7. **Clear Output** - Know what's happening without parsing logs
8. **Stateless CLI** - Server state derived from config, not stored in CLI
