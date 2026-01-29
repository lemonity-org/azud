package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// RegistryConfig holds registry authentication configuration
type RegistryConfig struct {
	// Registry server URL (e.g., docker.io, ghcr.io, gcr.io)
	Server string

	// Username
	Username string

	// Password or token
	Password string

	// Email (optional, some registries require it)
	Email string
}

// RegistryManager handles registry operations
type RegistryManager struct {
	client *Client
}

// NewRegistryManager creates a new registry manager
func NewRegistryManager(client *Client) *RegistryManager {
	return &RegistryManager{client: client}
}

// Login logs into a Docker registry
func (m *RegistryManager) Login(host string, config *RegistryConfig) error {
	server := config.Server
	if server == "" {
		server = "docker.io"
	}

	// Use --password-stdin for security
	cmd := fmt.Sprintf("echo %q | docker login --username %q --password-stdin %s",
		config.Password, config.Username, server)

	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("login failed: %s", result.Stderr)
	}

	return nil
}

// LoginAll logs into a registry on multiple hosts
func (m *RegistryManager) LoginAll(hosts []string, config *RegistryConfig) map[string]error {
	server := config.Server
	if server == "" {
		server = "docker.io"
	}

	cmd := fmt.Sprintf("echo %q | docker login --username %q --password-stdin %s",
		config.Password, config.Username, server)

	results := m.client.ssh.ExecuteParallel(hosts, cmd)
	errors := make(map[string]error)

	for _, result := range results {
		if !result.Success() {
			errors[result.Host] = fmt.Errorf("login failed: %s", result.Stderr)
		}
	}

	return errors
}

// Logout logs out from a Docker registry
func (m *RegistryManager) Logout(host, server string) error {
	if server == "" {
		server = "docker.io"
	}

	result, err := m.client.Execute(host, "logout", server)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("logout failed: %s", result.Stderr)
	}

	return nil
}

// LogoutAll logs out from a registry on multiple hosts
func (m *RegistryManager) LogoutAll(hosts []string, server string) map[string]error {
	if server == "" {
		server = "docker.io"
	}

	results := m.client.ExecuteAll(hosts, "logout", server)
	errors := make(map[string]error)

	for _, result := range results {
		if !result.Success() {
			errors[result.Host] = fmt.Errorf("logout failed: %s", result.Stderr)
		}
	}

	return errors
}

// IsLoggedIn checks if already logged into a registry
func (m *RegistryManager) IsLoggedIn(host, server string) (bool, error) {
	if server == "" {
		server = "docker.io"
	}

	// Check docker config for auth entry
	result, err := m.client.Execute(host, "cat", "~/.docker/config.json")
	if err != nil {
		return false, nil // Config doesn't exist, not logged in
	}

	if result.ExitCode != 0 {
		return false, nil
	}

	var config dockerConfigFile
	if err := json.Unmarshal([]byte(result.Stdout), &config); err != nil {
		return false, nil
	}

	// Check for auth entry
	serverKey := normalizeRegistry(server)
	_, hasAuth := config.Auths[serverKey]
	return hasAuth, nil
}

// GetAuthToken generates an auth token for a registry
func (m *RegistryManager) GetAuthToken(config *RegistryConfig) string {
	auth := config.Username + ":" + config.Password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

// dockerConfigFile represents the Docker config.json structure
type dockerConfigFile struct {
	Auths       map[string]authConfig `json:"auths"`
	CredsStore  string                `json:"credsStore,omitempty"`
	CredHelpers map[string]string     `json:"credHelpers,omitempty"`
}

type authConfig struct {
	Auth  string `json:"auth,omitempty"`
	Email string `json:"email,omitempty"`
}

// normalizeRegistry normalizes registry server names
func normalizeRegistry(server string) string {
	// Handle common registry aliases
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimPrefix(server, "http://")
	server = strings.TrimSuffix(server, "/")

	// Docker Hub has special handling
	if server == "docker.io" || server == "registry-1.docker.io" || server == "" {
		return "https://index.docker.io/v1/"
	}

	return server
}

// CommonRegistries holds configurations for common registries
var CommonRegistries = map[string]string{
	"dockerhub": "docker.io",
	"docker":    "docker.io",
	"ghcr":      "ghcr.io",
	"github":    "ghcr.io",
	"gcr":       "gcr.io",
	"google":    "gcr.io",
	"ecr":       "amazonaws.com",
	"aws":       "amazonaws.com",
	"acr":       "azurecr.io",
	"azure":     "azurecr.io",
	"quay":      "quay.io",
	"gitlab":    "registry.gitlab.com",
}

// ResolveRegistry resolves a registry alias to its server URL
func ResolveRegistry(name string) string {
	name = strings.ToLower(name)
	if server, ok := CommonRegistries[name]; ok {
		return server
	}
	return name
}

// ParseImageRef parses an image reference into registry, repository, and tag
func ParseImageRef(image string) (registry, repository, tag string) {
	// Default values
	registry = "docker.io"
	tag = "latest"

	// Split off tag/digest
	if idx := strings.LastIndex(image, ":"); idx != -1 && !strings.Contains(image[idx:], "/") {
		tag = image[idx+1:]
		image = image[:idx]
	} else if idx := strings.LastIndex(image, "@"); idx != -1 {
		tag = image[idx+1:]
		image = image[:idx]
	}

	// Split registry from repository
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		registry = parts[0]
		repository = parts[1]
	} else {
		// No registry specified, use docker.io
		if len(parts) == 1 {
			// Official image (e.g., "nginx")
			repository = "library/" + parts[0]
		} else {
			repository = image
		}
	}

	return registry, repository, tag
}

// BuildImageRef builds a full image reference from components
func BuildImageRef(registry, repository, tag string) string {
	if registry == "docker.io" {
		// For Docker Hub, we can omit the registry
		if strings.HasPrefix(repository, "library/") {
			repository = strings.TrimPrefix(repository, "library/")
		}
		if tag == "latest" {
			return repository
		}
		return repository + ":" + tag
	}

	if tag == "latest" {
		return registry + "/" + repository
	}
	return registry + "/" + repository + ":" + tag
}

// ECRLogin handles AWS ECR login which requires special handling
func (m *RegistryManager) ECRLogin(host, region, accountID string) error {
	// ECR login requires AWS CLI
	cmd := fmt.Sprintf(
		"aws ecr get-login-password --region %s | docker login --username AWS --password-stdin %s.dkr.ecr.%s.amazonaws.com",
		region, accountID, region,
	)

	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("ECR login failed: %s", result.Stderr)
	}

	return nil
}

// GCRLogin handles Google Container Registry login
func (m *RegistryManager) GCRLogin(host, keyFile string) error {
	cmd := fmt.Sprintf(
		"cat %s | docker login -u _json_key --password-stdin https://gcr.io",
		keyFile,
	)

	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("GCR login failed: %s", result.Stderr)
	}

	return nil
}
