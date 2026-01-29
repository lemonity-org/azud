package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adriancarayol/azud/internal/config"
)

// TempConfig creates a temporary config file and returns its path
// The file is automatically cleaned up when the test completes
func TempConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	return path
}

// TempSecrets creates a temporary secrets file and returns its path
func TempSecrets(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	secretsDir := filepath.Join(dir, ".azud")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatalf("Failed to create secrets dir: %v", err)
	}

	path := filepath.Join(secretsDir, "secrets")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write temp secrets: %v", err)
	}

	return dir
}

// MinimalConfig returns a minimal valid configuration for testing
func MinimalConfig() *config.Config {
	return &config.Config{
		Service: "test-service",
		Image:   "test-image:latest",
		Servers: map[string]config.RoleConfig{
			"web": {
				Hosts: []string{"localhost"},
			},
		},
		Proxy: config.ProxyConfig{
			Host:    "test.example.com",
			AppPort: 3000,
			Healthcheck: config.HealthcheckConfig{
				Path:     "/up",
				Interval: "1s",
				Timeout:  "5s",
			},
		},
		SSH: config.SSHConfig{
			User: "root",
			Port: 22,
		},
	}
}

// ConfigWithSSL returns a config with SSL enabled for testing
func ConfigWithSSL() *config.Config {
	cfg := MinimalConfig()
	cfg.Proxy.SSL = true
	cfg.Proxy.ACMEEmail = "test@example.com"
	return cfg
}

// ConfigWithCanary returns a config with canary deployments enabled
func ConfigWithCanary() *config.Config {
	cfg := MinimalConfig()
	cfg.Deploy.Canary = config.CanaryConfig{
		Enabled:       true,
		InitialWeight: 10,
		StepWeight:    10,
	}
	return cfg
}

// AssertNoError fails the test if err is not nil
func AssertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
}

// AssertError fails the test if err is nil
func AssertError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Error("Expected an error, got nil")
	}
}

// AssertEqual fails the test if expected != actual
func AssertEqual(t *testing.T, expected, actual interface{}) {
	t.Helper()
	if expected != actual {
		t.Errorf("Expected %v, got %v", expected, actual)
	}
}

// AssertContains fails the test if s does not contain substr
func AssertContains(t *testing.T, s, substr string) {
	t.Helper()
	if len(s) == 0 || len(substr) == 0 {
		t.Errorf("Expected %q to contain %q", s, substr)
		return
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return
		}
	}
	t.Errorf("Expected %q to contain %q", s, substr)
}

// MockSSHResult represents a mock SSH command result
type MockSSHResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// MockSSHClient is a mock implementation of SSH client for testing
type MockSSHClient struct {
	// ExecuteFunc is called when Execute is invoked
	ExecuteFunc func(host, cmd string) (*MockSSHResult, error)

	// Calls records all calls to Execute
	Calls []MockSSHCall
}

// MockSSHCall records a call to the mock SSH client
type MockSSHCall struct {
	Host    string
	Command string
}

// Execute implements the SSH client interface for testing
func (m *MockSSHClient) Execute(host, cmd string) (*MockSSHResult, error) {
	m.Calls = append(m.Calls, MockSSHCall{Host: host, Command: cmd})

	if m.ExecuteFunc != nil {
		return m.ExecuteFunc(host, cmd)
	}

	return &MockSSHResult{
		Stdout:   "",
		Stderr:   "",
		ExitCode: 0,
	}, nil
}

// Close is a no-op for the mock client
func (m *MockSSHClient) Close() error {
	return nil
}

// Reset clears all recorded calls
func (m *MockSSHClient) Reset() {
	m.Calls = nil
}
