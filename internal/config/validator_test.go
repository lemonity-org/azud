package config

import (
	"strings"
	"testing"
)

func TestValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty config",
			cfg:     &Config{},
			wantErr: true,
			errMsg:  "service",
		},
		{
			name: "missing image",
			cfg: &Config{
				Service: "test",
			},
			wantErr: true,
			errMsg:  "image",
		},
		{
			name: "missing servers",
			cfg: &Config{
				Service: "test",
				Image:   "test:latest",
			},
			wantErr: true,
			errMsg:  "servers",
		},
		{
			name: "valid minimal config",
			cfg: &Config{
				Service: "test",
				Image:   "test:latest",
				Servers: map[string]RoleConfig{
					"web": {Hosts: []string{"localhost"}},
				},
				SSH: SSHConfig{Port: 22},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestValidate_SSLRequiresACMEEmail(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host: "test.example.com",
			SSL:  true,
			// Missing ACMEEmail
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for SSL without ACME email")
		return
	}

	if !strings.Contains(err.Error(), "acme_email") {
		t.Errorf("expected error about acme_email, got: %v", err)
	}
}

func TestValidate_SSLWithACMEEmail(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host:      "test.example.com",
			SSL:       true,
			ACMEEmail: "admin@example.com",
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidate_CanaryConfig(t *testing.T) {
	tests := []struct {
		name    string
		canary  CanaryConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid canary config",
			canary: CanaryConfig{
				Enabled:       true,
				InitialWeight: 10,
				StepWeight:    10,
			},
			wantErr: false,
		},
		{
			name: "invalid initial weight",
			canary: CanaryConfig{
				Enabled:       true,
				InitialWeight: 150, // > 100
			},
			wantErr: true,
			errMsg:  "initial_weight",
		},
		{
			name: "negative initial weight",
			canary: CanaryConfig{
				Enabled:       true,
				InitialWeight: -10,
			},
			wantErr: true,
			errMsg:  "initial_weight",
		},
		{
			name: "invalid step weight",
			canary: CanaryConfig{
				Enabled:    true,
				StepWeight: 200,
			},
			wantErr: true,
			errMsg:  "step_weight",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Service: "test",
				Image:   "test:latest",
				Servers: map[string]RoleConfig{
					"web": {Hosts: []string{"localhost"}},
				},
				Deploy: DeployConfig{
					Canary: tt.canary,
				},
				SSH: SSHConfig{Port: 22},
			}

			err := Validate(cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestValidate_HostAddresses(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"valid IP", "192.168.1.1", false},
		{"valid hostname", "server.example.com", false},
		{"valid short hostname", "localhost", false},
		{"empty host", "", true},
		{"invalid chars", "server@example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Service: "test",
				Image:   "test:latest",
				Servers: map[string]RoleConfig{
					"web": {Hosts: []string{tt.host}},
				},
				SSH: SSHConfig{Port: 22},
			}

			err := Validate(cfg)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil && strings.Contains(err.Error(), "host") {
				t.Errorf("unexpected host error: %v", err)
			}
		})
	}
}

func TestValidate_PodmanConfig(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid netavark backend",
			backend: "netavark",
			wantErr: false,
		},
		{
			name:    "valid cni backend",
			backend: "cni",
			wantErr: false,
		},
		{
			name:    "empty backend (valid, uses default)",
			backend: "",
			wantErr: false,
		},
		{
			name:    "invalid backend",
			backend: "bridge",
			wantErr: true,
			errMsg:  "network_backend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Service: "test",
				Image:   "test:latest",
				Servers: map[string]RoleConfig{
					"web": {Hosts: []string{"localhost"}},
				},
				Podman: PodmanConfig{
					NetworkBackend: tt.backend,
				},
				SSH: SSHConfig{Port: 22},
			}

			err := Validate(cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestIsValidHost(t *testing.T) {
	tests := []struct {
		host  string
		valid bool
	}{
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"server.example.com", true},
		{"web-server-01.prod.example.com", true},
		{"", false},
		{"-invalid", false},
		{"invalid-", false},
		{"inv@lid", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			result := isValidHost(tt.host)
			if result != tt.valid {
				t.Errorf("isValidHost(%q) = %v, want %v", tt.host, result, tt.valid)
			}
		})
	}
}
