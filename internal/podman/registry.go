package podman

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// RegistryConfig holds registry authentication configuration.
type RegistryConfig struct {
	Server   string // e.g., docker.io, ghcr.io, gcr.io
	Username string
	Password string
	Email    string
}

// RegistryManager handles container registry operations via Podman.
type RegistryManager struct {
	client *Client
}

func NewRegistryManager(client *Client) *RegistryManager {
	return &RegistryManager{client: client}
}

func (m *RegistryManager) Login(host string, config *RegistryConfig) error {
	server := config.Server
	if server == "" {
		server = "docker.io"
	}

	cmd := fmt.Sprintf("echo %q | podman login --username %q --password-stdin %s",
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

func (m *RegistryManager) LoginAll(hosts []string, config *RegistryConfig) map[string]error {
	server := config.Server
	if server == "" {
		server = "docker.io"
	}

	cmd := fmt.Sprintf("echo %q | podman login --username %q --password-stdin %s",
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

func (m *RegistryManager) IsLoggedIn(host, server string) (bool, error) {
	if server == "" {
		server = "docker.io"
	}

	checkCmd := `cat ${XDG_RUNTIME_DIR}/containers/auth.json 2>/dev/null || cat ~/.config/containers/auth.json 2>/dev/null || cat /run/containers/0/auth.json 2>/dev/null`
	result, err := m.client.ssh.Execute(host, checkCmd)
	if err != nil {
		return false, nil // Auth file doesn't exist, not logged in
	}

	if result.ExitCode != 0 {
		return false, nil
	}

	var config containerAuthFile
	if err := json.Unmarshal([]byte(result.Stdout), &config); err != nil {
		return false, nil
	}

	serverKey := normalizeRegistry(server)
	_, hasAuth := config.Auths[serverKey]
	return hasAuth, nil
}

func (m *RegistryManager) GetAuthToken(config *RegistryConfig) string {
	auth := config.Username + ":" + config.Password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

type containerAuthFile struct {
	Auths       map[string]authConfig `json:"auths"`
	CredsStore  string                `json:"credsStore,omitempty"`
	CredHelpers map[string]string     `json:"credHelpers,omitempty"`
}

type authConfig struct {
	Auth  string `json:"auth,omitempty"`
	Email string `json:"email,omitempty"`
}

func normalizeRegistry(server string) string {
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimPrefix(server, "http://")
	server = strings.TrimSuffix(server, "/")

	if server == "docker.io" || server == "registry-1.docker.io" || server == "" {
		return "https://index.docker.io/v1/"
	}

	return server
}

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

func ResolveRegistry(name string) string {
	name = strings.ToLower(name)
	if server, ok := CommonRegistries[name]; ok {
		return server
	}
	return name
}

// ParseImageRef parses an image reference into registry, repository, and tag.
func ParseImageRef(image string) (registry, repository, tag string) {
	registry = "docker.io"
	tag = "latest"

	if idx := strings.LastIndex(image, ":"); idx != -1 && !strings.Contains(image[idx:], "/") {
		tag = image[idx+1:]
		image = image[:idx]
	} else if idx := strings.LastIndex(image, "@"); idx != -1 {
		tag = image[idx+1:]
		image = image[:idx]
	}

	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		registry = parts[0]
		repository = parts[1]
	} else {
		if len(parts) == 1 {
			repository = "library/" + parts[0]
		} else {
			repository = image
		}
	}

	return registry, repository, tag
}

func BuildImageRef(registry, repository, tag string) string {
	if registry == "docker.io" {
		repository = strings.TrimPrefix(repository, "library/")
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

// ECRLogin handles AWS ECR login via the AWS CLI.
func (m *RegistryManager) ECRLogin(host, region, accountID string) error {
	cmd := fmt.Sprintf(
		"aws ecr get-login-password --region %s | podman login --username AWS --password-stdin %s.dkr.ecr.%s.amazonaws.com",
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

// GCRLogin handles Google Container Registry login via a JSON key file.
func (m *RegistryManager) GCRLogin(host, keyFile string) error {
	cmd := fmt.Sprintf(
		"cat %s | podman login -u _json_key --password-stdin https://gcr.io",
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
