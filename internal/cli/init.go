package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a new deployment configuration",
	Long: `Initialize a new Azud deployment configuration.

This creates:
  - config/deploy.yml: Main deployment configuration
  - .azud/secrets: File to store secrets
  - .azud/hooks/: Directory for hook scripts

Options:
  --bundle        Use a template bundle (e.g., rails, node)
  --github-actions Create a GitHub Actions workflow for CI/CD

You can specify a bundle to use a template from the Azud registry.

Example:
  azud init
  azud init --github-actions
  azud init --bundle rails`,
	RunE: runInit,
}

var (
	initBundle        string
	initGitHubActions bool
)

func init() {
	initCmd.Flags().StringVar(&initBundle, "bundle", "", "Template bundle to use (e.g., rails, node)")
	initCmd.Flags().BoolVar(&initGitHubActions, "github-actions", false, "Include GitHub Actions workflow")
}

func runInit(cmd *cobra.Command, args []string) error {
	// Check if config already exists
	if existingConfig := findConfigFile(); existingConfig != "" {
		return fmt.Errorf("configuration already exists at %s", existingConfig)
	}

	// Create directories
	dirs := []string{
		"config",
		".azud",
		".azud/hooks",
	}

	if initGitHubActions {
		dirs = append(dirs, ".github/workflows")
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create deploy.yml
	configPath := filepath.Join("config", "deploy.yml")
	if err := os.WriteFile(configPath, []byte(getConfigTemplate()), 0644); err != nil {
		return fmt.Errorf("failed to create %s: %w", configPath, err)
	}
	fmt.Printf("Created %s\n", configPath)

	// Create secrets file
	secretsPath := filepath.Join(".azud", "secrets")
	if err := os.WriteFile(secretsPath, []byte(getSecretsTemplate()), 0600); err != nil {
		return fmt.Errorf("failed to create %s: %w", secretsPath, err)
	}
	fmt.Printf("Created %s\n", secretsPath)

	// Create sample hook scripts
	hooks := map[string]string{
		"pre-connect":       getPreConnectHook(),
		"pre-build":         getPreBuildHook(),
		"post-build":        getPostBuildHook(),
		"pre-deploy":        getPreDeployHook(),
		"pre-app-boot":      getPreAppBootHook(),
		"post-app-boot":     getPostAppBootHook(),
		"post-deploy":       getPostDeployHook(),
		"pre-proxy-reboot":  getPreProxyRebootHook(),
		"post-proxy-reboot": getPostProxyRebootHook(),
		"post-rollback":     getPostRollbackHook(),
	}

	for name, content := range hooks {
		hookPath := filepath.Join(".azud", "hooks", name)
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			return fmt.Errorf("failed to create hook %s: %w", hookPath, err)
		}
	}
	fmt.Printf("Created hooks in .azud/hooks/\n")

	// Create GitHub Actions workflow if requested
	if initGitHubActions {
		workflowPath := filepath.Join(".github", "workflows", "deploy.yml")
		if err := os.WriteFile(workflowPath, []byte(getGitHubActionsWorkflow()), 0644); err != nil {
			return fmt.Errorf("failed to create %s: %w", workflowPath, err)
		}
		fmt.Printf("Created %s\n", workflowPath)
	}

	// Add .azud/secrets to .gitignore if it exists
	if _, err := os.Stat(".gitignore"); err == nil {
		appendToGitignore()
	}

	fmt.Println()
	fmt.Println("Azud configuration initialized!")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit config/deploy.yml with your settings")
	fmt.Println("  2. Add your secrets to .azud/secrets")
	if initGitHubActions {
		fmt.Println("  3. Configure GitHub secrets (AZUD_SSH_KEY, AZUD_REGISTRY_PASSWORD)")
		fmt.Println("  4. Push to main branch to trigger deployment")
	} else {
		fmt.Println("  3. Run 'azud setup' to bootstrap servers and deploy")
		fmt.Println("")
		fmt.Println("For CI/CD deployment, run 'azud init --github-actions' in a new project")
	}

	return nil
}

func getConfigTemplate() string {
	return `# Azud Deployment Configuration
# Documentation: https://github.com/lemonity-org/azud

# Service name (used as container name prefix)
service: my-app

# Container image name (registry/user/image)
image: my-user/my-app

# Container registry configuration
registry:
  # Registry server (omit for Docker Hub)
  # server: ghcr.io
  username: my-user
  password:
    - AZUD_REGISTRY_PASSWORD

# Target servers organized by role
servers:
  # Web servers (handle HTTP traffic)
  web:
    hosts:
      - 192.168.1.1
    # Uncomment for custom labels
    # labels:
    #   app: my-app
    # Uncomment for resource limits
    # options:
    #   memory: "512m"
    #   cpus: "0.5"

  # Uncomment to add worker servers
  # workers:
  #   hosts:
  #     - 192.168.1.2
  #   cmd: "./bin/jobs"

# Proxy configuration (for zero-downtime deployments)
proxy:
  # Your application's hostname
  host: my-app.example.com
  # Enable automatic SSL via Let's Encrypt
  ssl: true
  # Application port inside the container
  app_port: 3000
  # Health check configuration
  healthcheck:
    path: /up
    interval: 1s
    timeout: 5s
    # readiness_path: /ready
    # liveness_path: /live
    # disable_liveness: true
    # liveness_cmd: "curl -fsS http://localhost:3000/up"
    # helper_image: "curlimages/curl:8.5.0"
    # helper_pull: "missing"

# Environment variables
env:
  # Non-secret environment variables
  clear:
    RAILS_ENV: production
    # Add your environment variables here
  # Secret environment variable names (values come from .azud/secrets)
  secret:
    - DATABASE_PASSWORD
    - RAILS_MASTER_KEY

# Accessories (databases, caches, etc.)
# Uncomment to add accessories
# accessories:
#   mysql:
#     image: mysql:8.0
#     host: 192.168.1.10
#     port: "3306:3306"
#     env:
#       clear:
#         MYSQL_DATABASE: my_app_production
#       secret:
#         - MYSQL_ROOT_PASSWORD
#     volumes:
#       - /var/lib/mysql:/var/lib/mysql
#
#   redis:
#     image: redis:7-alpine
#     host: 192.168.1.10
#     port: "6379:6379"
#     cmd: "redis-server --appendonly yes"

# SSH configuration
ssh:
  user: root
  # port: 22
  # keys:
  #   - ~/.ssh/id_rsa
  #   - ~/.ssh/id_ed25519
  # known_hosts_file: ~/.ssh/known_hosts
  # trusted_host_fingerprints:
  #   "192.168.1.1":
  #     - "SHA256:replace_with_fingerprint"
  # insecure_ignore_host_key: false
  # Uncomment for bastion/jump host
  # proxy:
  #   host: bastion.example.com
  #   user: deploy

# Secrets provider (optional)
# secrets_provider: file # file | env | command
# secrets_env_prefix: AZUD_SECRET_
# secrets_command: "printenv | grep '^AZUD_SECRET_' | sed 's/^AZUD_SECRET_//'"
# secrets_remote_path: $HOME/.azud/secrets

# Security policies (recommended for production)
# security:
#   require_non_root_ssh: true
#   require_rootless_podman: true
#   require_known_hosts: true
#   require_trusted_fingerprints: true

# Builder configuration
builder:
  # dockerfile: Dockerfile
  # context: .
  # Build arguments
  # args:
  #   RUBY_VERSION: "3.2"
  # Remote builder (for CI/CD)
  # remote:
  #   host: builder.example.com
  #   arch: amd64

# Deployment settings
deploy:
  # Wait before health checks
  readiness_delay: 7s
  # Maximum deployment time
  deploy_timeout: 30s
  # Time to drain connections
  drain_timeout: 30s
  # Old containers to keep
  retain_containers: 5

# Hooks configuration (optional)
# Hooks are discovered by filename in the hooks_path directory (.azud/hooks/).
# hooks:
#   timeout: 5m

# Volume mounts
# volumes:
#   - /app/storage:/app/storage

# Asset path for bridging between versions
# asset_path: /app/public/assets

# Cron jobs (scheduled tasks)
# cron:
#   db_backup:
#     schedule: "0 2 * * *"      # Daily at 2 AM
#     command: "bin/rails db:backup"
#     lock: true                  # Prevent overlapping runs
#
#   cleanup:
#     schedule: "0 4 * * 0"      # Weekly on Sunday at 4 AM
#     command: "bin/cleanup_old_files"
#     timeout: "1h"
#
#   send_reports:
#     schedule: "0 9 * * 1"      # Weekly on Monday at 9 AM
#     command: "bin/rails reports:send"
#     host: 192.168.1.1          # Run on specific host

# Command aliases
# aliases:
#   console: app exec --interactive -- bin/rails console
#   logs: app logs -f
`
}

func getSecretsTemplate() string {
	return `# Azud Secrets
# This file should NOT be committed to version control
# Add it to .gitignore

# Registry password
AZUD_REGISTRY_PASSWORD=

# Database credentials
DATABASE_PASSWORD=

# Application secrets
RAILS_MASTER_KEY=

# MySQL root password (if using mysql accessory)
# MYSQL_ROOT_PASSWORD=
`
}

func getPreConnectHook() string {
	return `#!/bin/sh
# Pre-connect hook — runs before connecting to servers
# Exit with non-zero to abort deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-connect hook..."
`
}

func getPreBuildHook() string {
	return `#!/bin/sh
# Pre-build hook — runs before building the container image
# Exit with non-zero to abort build
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference (with tag)
#   AZUD_VERSION      Image version/tag
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-build hook..."
`
}

func getPostBuildHook() string {
	return `#!/bin/sh
# Post-build hook — runs after successful build, before push
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference (with tag)
#   AZUD_VERSION      Image version/tag
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-build hook..."
`
}

func getPreDeployHook() string {
	return `#!/bin/sh
# Pre-deploy hook — runs before deploying to servers
# Exit with non-zero to abort deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-deploy hook..."
`
}

func getPreAppBootHook() string {
	return `#!/bin/sh
# Pre-app-boot hook — runs locally before starting a new container on each host.
# This hook executes on the machine running azud, NOT on the remote host.
# AZUD_HOSTS contains the target host being deployed to.
# Exit with non-zero to abort deployment on this host.
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target host being deployed to
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-app-boot hook..."
`
}

func getPostAppBootHook() string {
	return `#!/bin/sh
# Post-app-boot hook — runs locally after container passes health check on each host.
# This hook executes on the machine running azud, NOT on the remote host.
# AZUD_HOSTS contains the target host that was deployed to.
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target host that was deployed to
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-app-boot hook..."
`
}

func getPostDeployHook() string {
	return `#!/bin/sh
# Post-deploy hook — runs after successful deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)
#   AZUD_RUNTIME      Deployment duration in seconds

echo "Running post-deploy hook..."
echo "Deployment complete!"
`
}

func getPreProxyRebootHook() string {
	return `#!/bin/sh
# Pre-proxy-reboot hook — runs before booting/rebooting the proxy
# Exit with non-zero to abort proxy reboot
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-proxy-reboot hook..."
`
}

func getPostProxyRebootHook() string {
	return `#!/bin/sh
# Post-proxy-reboot hook — runs after proxy boot/reboot completes
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-proxy-reboot hook..."
`
}

func getPostRollbackHook() string {
	return `#!/bin/sh
# Post-rollback hook — runs after rollback completes
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Version rolled back to
#   AZUD_HOSTS        Hosts that were rolled back (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-rollback hook..."
`
}

func appendToGitignore() {
	content, err := os.ReadFile(".gitignore")
	if err != nil {
		return
	}

	if strings.Contains(string(content), ".azud/secrets") {
		return
	}

	// Append to .gitignore
	f, err := os.OpenFile(".gitignore", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = f.WriteString("\n# Azud secrets\n.azud/secrets\n")
	fmt.Println("Added .azud/secrets to .gitignore")
}

func getGitHubActionsWorkflow() string {
	return `# Azud Deployment Workflow
# This workflow deploys your application when pushing to main.
#
# Required GitHub secrets:
#   AZUD_SSH_KEY              - SSH private key for server access
#   KNOWN_HOSTS               - Output of: ssh-keyscan your-server-ip
#   AZUD_REGISTRY_PASSWORD    - Container registry password/token
#   DATABASE_PASSWORD          - (Example) Application secrets
#
# Your config/deploy.yml should include:
#   secrets_provider: env
#   secrets_env_prefix: AZUD_SECRET_

name: Deploy

on:
  push:
    branches: [main]
  workflow_dispatch:

jobs:
  deploy:
    name: Deploy Application
    runs-on: ubuntu-latest

    steps:
      - name: Checkout code
        uses: actions/checkout@v6

      - name: Setup Azud
        uses: lemonity-org/azud@v1
        with:
          ssh-key: ${{ secrets.AZUD_SSH_KEY }}
          known-hosts: ${{ secrets.KNOWN_HOSTS }}
          registry-server: ghcr.io
          registry-username: ${{ github.actor }}
          registry-password: ${{ secrets.GITHUB_TOKEN }}

      - name: Deploy
        env:
          AZUD_SECRET_AZUD_REGISTRY_PASSWORD: ${{ secrets.AZUD_REGISTRY_PASSWORD }}
          AZUD_SECRET_DATABASE_PASSWORD: ${{ secrets.DATABASE_PASSWORD }}
        run: azud deploy

  # Optional: Run tests before deploying
  # test:
  #   name: Run Tests
  #   runs-on: ubuntu-latest
  #   steps:
  #     - uses: actions/checkout@v6
  #     - name: Run tests
  #       run: |
  #         # Add your test commands here
  #         npm test

  # Optional: Deploy to staging first
  # staging:
  #   name: Deploy to Staging
  #   runs-on: ubuntu-latest
  #   environment: staging
  #   needs: [test]
  #   steps:
  #     - uses: actions/checkout@v6
  #     - name: Setup Azud
  #       uses: lemonity-org/azud@v1
  #       with:
  #         ssh-key: ${{ secrets.AZUD_SSH_KEY }}
  #         known-hosts: ${{ secrets.KNOWN_HOSTS }}
  #     - name: Deploy to staging
  #       run: azud deploy -d staging
`
}
