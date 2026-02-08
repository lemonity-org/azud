# Azud FAQ

## General

### What is Azud?
Azud is a command-line tool for deploying containerized web applications. It's designed to be a middle ground between simple single-server tools (like Dokku) and complex orchestration platforms (like Kubernetes). It uses **Podman** for running containers and **Caddy** as a reverse proxy to manage traffic and provide automatic SSL.

### How should I manage secrets?
Keep secrets out of version control. Store them in `.azud/secrets` locally and add it to `.gitignore`. In CI/CD, reconstruct the file at runtime from your CI secret store, or pass secrets through environment variables that your `config/deploy.yml` references.

### Why not just use Docker Compose?
Docker Compose is great for local development and single-server setups, but it lacks built-in features for:
- Zero-downtime deployments (rolling updates)
- Multi-server orchestration
- Automated health checks and traffic switching
- Automatic SSL certificate management across multiple domains

Azud handles all of this while keeping the configuration nearly as simple as a Compose file.

### Why not Kubernetes?
Kubernetes is powerful but introduces significant complexity (control plane, networking overlay, storage classes, etc.). For many applications, you just need to run containers on a few servers with zero downtime and SSL. Azud provides this "PaaS-like" experience on your own hardware without the overhead of k8s.

---

## Technical Architecture

### Why Podman instead of Docker?
Azud uses Podman because:
- **Daemonless:** No central daemon running as root, improving security and stability.
- **Rootless:** Containers can run as a non-root user, which is a major security advantage.
- **Systemd Integration:** Podman integrates natively with systemd (via Quadlet), making it reliable for process management and auto-start on boot.

### Do I need to install anything on my servers?
Yes, but Azud handles most of it for you. The `azud setup` (or `azud systemd enable`) command will ensure the necessary components are installed.
You mainly need:
- A Linux server (Ubuntu, Debian, CentOS, etc.)
- SSH access
- Podman installed (Azud can help bootstrap this)

### How does zero-downtime deployment work?
Azud implements a **Blue-Green** deployment strategy:
1.  **Start:** New containers (Green) are started alongside the old ones (Blue).
2.  **Wait:** Azud waits for the new containers to pass their health checks.
3.  **Switch:** The Caddy proxy is updated to route traffic to the new containers.
4.  **Drain:** Old containers are kept running briefly to finish existing requests.
5.  **Stop:** Old containers are stopped and removed.

---

## Networking & Proxy

### How does SSL/HTTPS work?
Azud deploys **Caddy** as the ingress proxy. Caddy automatically requests and renews certificates from Let's Encrypt for all domains defined in your `config/deploy.yml`. You just point your DNS to the server, and Azud handles the rest.

### Can I use my own load balancer (AWS ALB, Cloudflare)?
Yes.
- **Cloudflare:** Set `ssl: false` in `proxy` config if you are using Cloudflare's strict SSL or want to terminate SSL at Cloudflare.
- **AWS ALB:** You can point your ALB to the servers. Note that Azud manages ports dynamically for containers, but the Caddy proxy listens on static ports (80/443), so your ALB should forward to those.

### What if I have multiple apps on one server?
Azud is designed to host multiple apps on the same server/cluster. The Caddy proxy is shared (or can be configured per-service if needed, though a shared ingress is typical). Each app just needs a unique name and distinct domains/paths.

**Important:** If multiple services share a host, set a unique `secrets_remote_path` per service so they don't overwrite each other's secrets file (the default is `$HOME/.azud/secrets` for all services):

```yaml
# config/deploy.yml (service A)
secrets_remote_path: $HOME/.azud/my-api/secrets

# config/deploy.yml (service B)
secrets_remote_path: $HOME/.azud/my-web/secrets
```

Then run `azud env push` from each project to sync secrets to the correct path.

---

## Troubleshooting

### My deployment failed. How do I debug?
1.  **Check logs:** `azud app logs --tail 200`
2.  **Check details:** `azud app details` to see container status.
3.  **Verbose mode:** Run your command with `-v` (e.g., `azud deploy -v`) to see detailed SSH output and error messages.

### How do I rollback?
Run `azud history list` to find a previous version, then `azud rollback <version>`.

### The health check is failing.
- Ensure your app is actually listening on the `app_port` defined in `config/deploy.yml`.
- Check if your `healthcheck.path` (default `/up`) actually returns a 200 OK status code.
- Increase `readiness_delay` in `deploy` config if your app takes a long time to boot.

---

## Comparisons

### Azud vs. Kamal
- **Proxy:** Azud uses **Caddy** (auto-HTTPS, robust) vs Kamal's custom `kamal-proxy`.
- **SSL:** Azud has built-in Let's Encrypt. Kamal often requires a separate load balancer or proxy for SSL.
- **Output:** Azud focuses on clean, human-readable output.
- **State:** Azud uses a robust state management approach to avoid "missing container" issues.

### Azud vs. Dokku
- **Multi-Server:** Azud is multi-server by default. Dokku is primarily single-server.
- **Architecture:** Azud is client-side (CLI tool), pushing commands over SSH. Dokku is server-side (git push to server).
- **Control:** Azud gives you more direct control over the deployment process and orchestration across multiple nodes.

## Related docs

- `docs/GETTING_STARTED.md`
- `docs/HOW_IT_WORKS.md`
