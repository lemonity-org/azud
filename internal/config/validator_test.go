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
				Proxy: ProxyConfig{
					Host: "test.example.com",
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
				Proxy: ProxyConfig{
					Host: "test.example.com",
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
				Proxy: ProxyConfig{
					Host: "test.example.com",
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

func TestValidate_ProxyHosts(t *testing.T) {
	tests := []struct {
		name      string
		hosts     []string
		host      string
		wantErr   bool
		errTarget string
	}{
		{
			name:    "valid proxy host list",
			hosts:   []string{"example.com", "www.example.com"},
			wantErr: false,
		},
		{
			name:      "invalid proxy host",
			host:      "bad@host",
			wantErr:   true,
			errTarget: "proxy.host",
		},
		{
			name:      "invalid proxy hosts entry",
			hosts:     []string{"example.com", "bad@host"},
			wantErr:   true,
			errTarget: "proxy.hosts[1]",
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
				Proxy: ProxyConfig{
					Host:  tt.host,
					Hosts: tt.hosts,
				},
				SSH: SSHConfig{Port: 22},
			}

			err := Validate(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errTarget)
				}
				if tt.errTarget != "" && !strings.Contains(err.Error(), tt.errTarget) {
					t.Fatalf("expected error containing %q, got %q", tt.errTarget, err.Error())
				}
			} else if err != nil {
				t.Fatalf("expected no error, got %v", err)
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
				Proxy: ProxyConfig{
					Host: "test.example.com",
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

func TestValidate_TrustedFingerprintsIncludeAllSSHHosts(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"web.example.com"}},
		},
		Accessories: map[string]AccessoryConfig{
			"redis": {
				Image: "redis:7",
				Host:  "redis.example.com",
			},
		},
		Cron: map[string]CronConfig{
			"job": {
				Host:     "cron.example.com",
				Schedule: "0 * * * *",
				Command:  "echo ok",
			},
		},
		Builder: BuilderConfig{
			Remote: RemoteBuilderConfig{
				Host: "builder.example.com",
			},
		},
		SSH: SSHConfig{
			Port: 22,
			Proxy: SSHProxyConfig{
				Host: "bastion.example.com",
			},
			TrustedHostFingerprints: map[string][]string{
				"web.example.com":     {"SHA256:abc"},
				"redis.example.com":   {"SHA256:def"},
				"cron.example.com":    {"SHA256:ghi"},
				"builder.example.com": {"SHA256:jkl"},
				"bastion.example.com": {"SHA256:mno"},
			},
		},
		Proxy: ProxyConfig{
			Host: "app.example.com",
		},
		Security: SecurityConfig{
			RequireTrustedFingerprints: true,
		},
	}

	if err := Validate(cfg); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	cfg.SSH.TrustedHostFingerprints = map[string][]string{
		"web.example.com": {"SHA256:abc"},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "missing fingerprint") {
		t.Fatalf("expected missing fingerprint error, got %v", err)
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

func TestValidate_ExtraHostAddresses(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host: "test.example.com",
		},
		Accessories: map[string]AccessoryConfig{
			"redis": {
				Image: "redis:7",
				Host:  "bad@host",
			},
		},
		Cron: map[string]CronConfig{
			"job": {
				Host: "cron@host",
			},
		},
		SSH: SSHConfig{
			Port: 22,
			Proxy: SSHProxyConfig{
				Host: "proxy@host",
			},
		},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "accessories.redis.host") {
		t.Fatalf("expected accessory host error, got %v", err)
	}
	if !strings.Contains(err.Error(), "cron.job.host") {
		t.Fatalf("expected cron host error, got %v", err)
	}
	if !strings.Contains(err.Error(), "ssh.proxy.host") {
		t.Fatalf("expected ssh proxy host error, got %v", err)
	}
}

func TestValidate_ProxyRangesAndDurations(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host:            "test.example.com",
			HTTPPort:        70000,
			HTTPSPort:       -1,
			ResponseTimeout: "notaduration",
			Healthcheck: HealthcheckConfig{
				Interval: "bad",
				Timeout:  "bad",
			},
			Buffering: BufferingConfig{
				MaxRequestBody: -1,
				Memory:         -1,
			},
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}
	for _, field := range []string{
		"proxy.http_port",
		"proxy.https_port",
		"proxy.response_timeout",
		"proxy.healthcheck.interval",
		"proxy.healthcheck.timeout",
		"proxy.buffering.max_request_body",
		"proxy.buffering.memory",
	} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("expected error containing %q, got %v", field, err)
		}
	}
}

func TestValidate_CronRequiredFields(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host: "test.example.com",
		},
		Cron: map[string]CronConfig{
			"job": {
				Schedule: "",
				Command:  "",
				Timeout:  "notaduration",
			},
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}
	for _, field := range []string{
		"cron.job.schedule",
		"cron.job.command",
		"cron.job.timeout",
	} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("expected error containing %q, got %v", field, err)
		}
	}
}

// New tests for the 16 issues

func TestIsValidCronSchedule(t *testing.T) {
	tests := []struct {
		schedule string
		valid    bool
	}{
		{"0 * * * *", true},
		{"*/5 * * * *", true},
		{"0 0 * * *", true},
		{"0 0 1 1 *", true},
		{"0 0 * * 0", true},
		{"0 0 * * 7", true},
		{"1,15,30 * * * *", true},
		{"0 1-5 * * *", true},
		{"0 1-5/2 * * *", true},
		{"@daily", true},
		{"@hourly", true},
		{"@weekly", true},
		{"@monthly", true},
		{"@yearly", true},
		{"@annually", true},
		{"@midnight", true},

		// Invalid
		{"", false},
		{"bad", false},
		{"0 * *", false},               // too few fields
		{"0 * * * * *", false},          // too many fields
		{"60 * * * *", false},           // minute out of range
		{"0 24 * * *", false},           // hour out of range
		{"0 0 0 * *", false},            // day-of-month out of range
		{"0 0 * 13 *", false},           // month out of range
		{"0 0 * * 8", false},            // day-of-week out of range
		{"0 0 * * abc", false},          // non-numeric
		{"0 0 5-3 * *", false},          // reversed range
	}

	for _, tt := range tests {
		t.Run(tt.schedule, func(t *testing.T) {
			result := isValidCronSchedule(tt.schedule)
			if result != tt.valid {
				t.Errorf("isValidCronSchedule(%q) = %v, want %v", tt.schedule, result, tt.valid)
			}
		})
	}
}

func TestValidate_CronScheduleSyntax(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host: "test.example.com",
		},
		Cron: map[string]CronConfig{
			"bad-schedule": {
				Schedule: "invalid cron",
				Command:  "echo ok",
			},
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected validation error for invalid cron schedule")
	}
	if !strings.Contains(err.Error(), "invalid cron schedule") {
		t.Fatalf("expected 'invalid cron schedule' error, got %v", err)
	}
}

func TestValidate_CacheTypeSpecificOptions(t *testing.T) {
	base := func() *Config {
		return &Config{
			Service: "test",
			Image:   "test:latest",
			Servers: map[string]RoleConfig{
				"web": {Hosts: []string{"localhost"}},
			},
			Proxy: ProxyConfig{Host: "test.example.com"},
			SSH:   SSHConfig{Port: 22},
		}
	}

	t.Run("registry missing ref", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "registry"
		cfg.Builder.Cache.Options = map[string]string{"mode": "max"}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "'ref'") {
			t.Fatalf("expected ref requirement error, got %v", err)
		}
	})

	t.Run("registry with ref", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "registry"
		cfg.Builder.Cache.Options = map[string]string{"ref": "myrepo:cache"}
		err := Validate(cfg)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("local missing src", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "local"
		cfg.Builder.Cache.Options = map[string]string{"dest": "/tmp/cache"}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "'src'") {
			t.Fatalf("expected src requirement error, got %v", err)
		}
	})

	t.Run("local missing dest", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "local"
		cfg.Builder.Cache.Options = map[string]string{"src": "/tmp/cache"}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "'dest'") {
			t.Fatalf("expected dest requirement error, got %v", err)
		}
	})

	t.Run("local with src and dest", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "local"
		cfg.Builder.Cache.Options = map[string]string{"src": "/tmp/src", "dest": "/tmp/dest"}
		err := Validate(cfg)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("gha missing scope", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "gha"
		cfg.Builder.Cache.Options = map[string]string{"url": "https://example.com"}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "'scope'") {
			t.Fatalf("expected scope requirement error, got %v", err)
		}
	})

	t.Run("gha with scope", func(t *testing.T) {
		cfg := base()
		cfg.Builder.Cache.Type = "gha"
		cfg.Builder.Cache.Options = map[string]string{"scope": "main"}
		err := Validate(cfg)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})
}

func TestIsValidPlatform(t *testing.T) {
	tests := []struct {
		platform string
		valid    bool
	}{
		{"linux/amd64", true},
		{"linux/arm64", true},
		{"linux/arm/v7", true},
		{"darwin/amd64", true},
		{"windows/amd64", true},

		{"", false},
		{"linux", false},
		{"linux/unknown", false},
		{"invalid/amd64", false},
		{"Linux/amd64", false}, // uppercase
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			result := isValidPlatform(tt.platform)
			if result != tt.valid {
				t.Errorf("isValidPlatform(%q) = %v, want %v", tt.platform, result, tt.valid)
			}
		})
	}
}

func TestValidate_BuilderPlatforms(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
		Builder: BuilderConfig{
			Platforms: []string{"linux/amd64", "bogus/platform"},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "builder.platforms[1]") {
		t.Fatalf("expected platforms validation error, got %v", err)
	}
}

func TestIsValidBuilderSecret(t *testing.T) {
	tests := []struct {
		spec  string
		valid bool
	}{
		{"my_secret", true},
		{"my-secret", true},
		{"SECRET123", true},
		{"id=mysecret,src=/path/to/file", true},
		{"id=mysecret,env=MY_VAR", true},

		{"", false},
		{"bad secret name!", false},
		{"id=mysecret", false},          // missing src or env
		{"src=/path", false},            // missing id
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			result := isValidBuilderSecret(tt.spec)
			if result != tt.valid {
				t.Errorf("isValidBuilderSecret(%q) = %v, want %v", tt.spec, result, tt.valid)
			}
		})
	}
}

func TestIsValidBuilderSSH(t *testing.T) {
	tests := []struct {
		spec  string
		valid bool
	}{
		{"default", true},
		{"id=mykey,src=/path/to/key", true},
		{"id=mykey", true},

		{"", false},
		{"notaspec", false},
		{"src=/path", false}, // missing id
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			result := isValidBuilderSSH(tt.spec)
			if result != tt.valid {
				t.Errorf("isValidBuilderSSH(%q) = %v, want %v", tt.spec, result, tt.valid)
			}
		})
	}
}

func TestValidate_BuilderRemoteArch(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
		Builder: BuilderConfig{
			Remote: RemoteBuilderConfig{
				Host: "builder.example.com",
				Arch: "mips", // invalid
			},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "builder.remote.arch") {
		t.Fatalf("expected builder.remote.arch error, got %v", err)
	}

	// Valid arch
	cfg.Builder.Remote.Arch = "amd64"
	err = Validate(cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestIsValidHeaderName(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"Content-Type", true},
		{"X-Custom-Header", true},
		{"Accept", true},
		{"x-request-id", true},

		{"", false},
		{" ", false},
		{"Header Name", false},  // space
		{"Header\tName", false}, // tab
		{"Header,Name", false},  // delimiter
		{"Header/Name", false},  // delimiter
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidHeaderName(tt.name)
			if result != tt.valid {
				t.Errorf("isValidHeaderName(%q) = %v, want %v", tt.name, result, tt.valid)
			}
		})
	}
}

func TestValidate_LoggingHeaders(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host: "test.example.com",
			Logging: LoggingConfig{
				RedactRequestHeaders:  []string{"Authorization", ""},
				RedactResponseHeaders: []string{"Set-Cookie"},
			},
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "redact_request_headers[1]") {
		t.Fatalf("expected header validation error, got %v", err)
	}
}

func TestIsValidSemver(t *testing.T) {
	tests := []struct {
		version string
		valid   bool
	}{
		{"1.0.0", true},
		{"0.1.0", true},
		{"v1.2.3", true},
		{"10.20.30", true},

		{"", false},
		{"1.0", false},
		{"1.0.0.0", false},
		{"v1.x.0", false},
		{"latest", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			result := isValidSemver(tt.version)
			if result != tt.valid {
				t.Errorf("isValidSemver(%q) = %v, want %v", tt.version, result, tt.valid)
			}
		})
	}
}

func TestValidateMinimumVersion(t *testing.T) {
	tests := []struct {
		name           string
		minVersion     string
		currentVersion string
		wantErr        bool
	}{
		{"no minimum", "", "1.0.0", false},
		{"dev bypasses", "2.0.0", "dev", false},
		{"equal version", "1.0.0", "1.0.0", false},
		{"current newer major", "1.0.0", "2.0.0", false},
		{"current newer minor", "1.0.0", "1.1.0", false},
		{"current newer patch", "1.0.0", "1.0.1", false},
		{"current older major", "2.0.0", "1.0.0", true},
		{"current older minor", "1.2.0", "1.1.0", true},
		{"current older patch", "1.0.2", "1.0.1", true},
		{"v prefix minimum", "v1.0.0", "1.0.0", false},
		{"v prefix current", "1.0.0", "v1.0.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{MinimumVersion: tt.minVersion}
			err := ValidateMinimumVersion(cfg, tt.currentVersion)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidate_MinimumVersionFormat(t *testing.T) {
	cfg := &Config{
		Service:        "test",
		Image:          "test:latest",
		MinimumVersion: "not-semver",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "minimum_version") {
		t.Fatalf("expected minimum_version format error, got %v", err)
	}
}

func TestIsValidImageRef(t *testing.T) {
	tests := []struct {
		image string
		valid bool
	}{
		{"nginx", true},
		{"nginx:latest", true},
		{"nginx:1.25.0", true},
		{"ghcr.io/org/repo:v1.0", true},
		{"localhost:5000/myimage", true},
		{"docker.io/library/redis:7-alpine", true},
		{"myimage@sha256:abcdef1234567890", true},
		{"registry.example.com/image:tag", true},

		{"", false},
		{"image with spaces", false},
		{"image:tag with spaces", false},
		{"image:tag!invalid", false},
		{"image@baddigest", false},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			result := isValidImageRef(tt.image)
			if result != tt.valid {
				t.Errorf("isValidImageRef(%q) = %v, want %v", tt.image, result, tt.valid)
			}
		})
	}
}

func TestValidate_ImageRefValidation(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "image with spaces",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid image reference") {
		t.Fatalf("expected image reference error, got %v", err)
	}
}

func TestValidate_AccessoryImageRef(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
		Accessories: map[string]AccessoryConfig{
			"db": {
				Image: "bad image!",
				Host:  "localhost",
			},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "accessories.db.image") {
		t.Fatalf("expected accessory image error, got %v", err)
	}
}

func TestValidate_ResponseHeaderTimeout(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{
			Host:                  "test.example.com",
			ResponseHeaderTimeout: "notaduration",
		},
		SSH: SSHConfig{Port: 22},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "response_header_timeout") {
		t.Fatalf("expected response_header_timeout error, got %v", err)
	}
}

func TestValidate_BuilderSecretsFormat(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
		Builder: BuilderConfig{
			Secrets: []string{"valid_secret", "bad secret!"},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "builder.secrets[1]") {
		t.Fatalf("expected builder secrets validation error, got %v", err)
	}
}

func TestValidate_BuilderSSHFormat(t *testing.T) {
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{
			"web": {Hosts: []string{"localhost"}},
		},
		Proxy: ProxyConfig{Host: "test.example.com"},
		SSH:   SSHConfig{Port: 22},
		Builder: BuilderConfig{
			SSH: []string{"default", "notaspec"},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "builder.ssh[1]") {
		t.Fatalf("expected builder SSH validation error, got %v", err)
	}
}

func TestValidate_CronHostResolution(t *testing.T) {
	// Cron job with no explicit host and no servers at all should fail
	cfg := &Config{
		Service: "test",
		Image:   "test:latest",
		Servers: map[string]RoleConfig{},
		Proxy:   ProxyConfig{Host: "test.example.com"},
		SSH:     SSHConfig{Port: 22},
		Cron: map[string]CronConfig{
			"cleanup": {
				Schedule: "0 0 * * *",
				Command:  "echo cleanup",
			},
		},
	}

	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "no host can be resolved") {
		t.Fatalf("expected cron host resolution error, got %v", err)
	}

	// Adding a web server should resolve the default
	cfg.Servers["web"] = RoleConfig{Hosts: []string{"web1.example.com"}}
	err = Validate(cfg)
	if err != nil && strings.Contains(err.Error(), "no host can be resolved") {
		t.Fatalf("expected cron host to resolve via web role, got %v", err)
	}
}
