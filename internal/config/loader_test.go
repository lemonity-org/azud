package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeConfigs_AllowsExplicitFalseAndZero(t *testing.T) {
	base := &Config{
		Proxy: ProxyConfig{
			SSL:         true,
			SSLRedirect: true,
			AppPort:     3000,
			Rootful:     true,
		},
		Deploy: DeployConfig{
			RollbackOnFailure: true,
			RetainContainers:  5,
		},
		Podman: PodmanConfig{
			Rootless: true,
		},
		Security: SecurityConfig{
			RequireNonRootSSH:          true,
			RequireRootlessPodman:      true,
			RequireKnownHosts:          true,
			RequireTrustedFingerprints: true,
		},
	}

	destYAML := []byte(`
proxy:
  ssl: false
  ssl_redirect: false
  app_port: 0
  rootful: false
deploy:
  rollback_on_failure: false
  retain_containers: 0
podman:
  rootless: false
security:
  require_non_root_ssh: false
  require_rootless_podman: false
  require_known_hosts: false
  require_trusted_fingerprints: false
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if merged.Proxy.SSL {
		t.Fatalf("expected proxy.ssl to be overridden to false")
	}
	if merged.Proxy.SSLRedirect {
		t.Fatalf("expected proxy.ssl_redirect to be overridden to false")
	}
	if merged.Proxy.AppPort != 0 {
		t.Fatalf("expected proxy.app_port to be overridden to 0")
	}
	if merged.Proxy.Rootful {
		t.Fatalf("expected proxy.rootful to be overridden to false")
	}
	if merged.Deploy.RollbackOnFailure {
		t.Fatalf("expected deploy.rollback_on_failure to be overridden to false")
	}
	if merged.Deploy.RetainContainers != 0 {
		t.Fatalf("expected deploy.retain_containers to be overridden to 0")
	}
	if merged.Podman.Rootless {
		t.Fatalf("expected podman.rootless to be overridden to false")
	}
	if merged.Security.RequireNonRootSSH || merged.Security.RequireRootlessPodman || merged.Security.RequireKnownHosts || merged.Security.RequireTrustedFingerprints {
		t.Fatalf("expected security booleans to be overridden to false")
	}
}

func TestMergeConfigs_ClearVolumes(t *testing.T) {
	base := &Config{
		Volumes: []string{"/data:/data", "/logs:/logs"},
	}

	destYAML := []byte(`
volumes: []
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Volumes) != 0 {
		t.Fatalf("expected volumes to be cleared, got %v", merged.Volumes)
	}
}

func TestMergeConfigs_ClearEnvSecret(t *testing.T) {
	base := &Config{
		Env: EnvConfig{
			Secret: []string{"DB_PASSWORD", "API_KEY"},
		},
	}

	destYAML := []byte(`
env:
  secret: []
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Env.Secret) != 0 {
		t.Fatalf("expected env.secret to be cleared, got %v", merged.Env.Secret)
	}
}

func TestMergeConfigs_ClearEnvClear(t *testing.T) {
	base := &Config{
		Env: EnvConfig{
			Clear: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
	}

	destYAML := []byte(`
env:
  clear: {}
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Env.Clear) != 0 {
		t.Fatalf("expected env.clear to be cleared, got %v", merged.Env.Clear)
	}
}

func TestMergeConfigs_ClearBuilderSecrets(t *testing.T) {
	base := &Config{
		Builder: BuilderConfig{
			Secrets: []string{"secret1", "secret2"},
		},
	}

	destYAML := []byte(`
builder:
  secrets: []
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Builder.Secrets) != 0 {
		t.Fatalf("expected builder.secrets to be cleared, got %v", merged.Builder.Secrets)
	}
}

func TestMergeConfigs_ClearBuilderCacheOptions(t *testing.T) {
	base := &Config{
		Builder: BuilderConfig{
			Cache: CacheConfig{
				Options: map[string]string{"ref": "old:cache"},
			},
		},
	}

	destYAML := []byte(`
builder:
  cache:
    options: {}
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Builder.Cache.Options) != 0 {
		t.Fatalf("expected builder.cache.options to be cleared, got %v", merged.Builder.Cache.Options)
	}
}

func TestMergeConfigs_ClearBuilderArgs(t *testing.T) {
	base := &Config{
		Builder: BuilderConfig{
			Args: map[string]string{"ARG1": "val1"},
		},
	}

	destYAML := []byte(`
builder:
  args: {}
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Builder.Args) != 0 {
		t.Fatalf("expected builder.args to be cleared, got %v", merged.Builder.Args)
	}
}

func TestMergeConfigs_ClearAliases(t *testing.T) {
	base := &Config{
		Aliases: map[string]string{"deploy": "push"},
	}

	destYAML := []byte(`
aliases: {}
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Aliases) != 0 {
		t.Fatalf("expected aliases to be cleared, got %v", merged.Aliases)
	}
}

func TestMergeConfigs_ClearTrustedHostFingerprints(t *testing.T) {
	base := &Config{
		SSH: SSHConfig{
			TrustedHostFingerprints: map[string][]string{
				"host1": {"SHA256:abc"},
			},
		},
	}

	destYAML := []byte(`
ssh:
  trusted_host_fingerprints: {}
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.SSH.TrustedHostFingerprints) != 0 {
		t.Fatalf("expected trusted_host_fingerprints to be cleared, got %v", merged.SSH.TrustedHostFingerprints)
	}
}

func TestMergeConfigs_ReplaceVolumesWithNewValues(t *testing.T) {
	base := &Config{
		Volumes: []string{"/data:/data", "/logs:/logs"},
	}

	destYAML := []byte(`
volumes:
  - /newdata:/newdata
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if len(merged.Volumes) != 1 || merged.Volumes[0] != "/newdata:/newdata" {
		t.Fatalf("expected volumes to be replaced, got %v", merged.Volumes)
	}
}

func TestMergeConfigs_VolumesFallbackAppend(t *testing.T) {
	base := &Config{
		Volumes: []string{"/data:/data"},
	}

	// Without a YAML node, the merge falls back to append behavior
	dest := &Config{
		Volumes: []string{"/logs:/logs"},
	}

	merged := mergeConfigs(base, dest, nil)
	if len(merged.Volumes) != 2 {
		t.Fatalf("expected volumes to be appended, got %v", merged.Volumes)
	}
}

func TestMergeConfigs_LoggingEnabledOverride(t *testing.T) {
	base := &Config{
		Proxy: ProxyConfig{
			Logging: LoggingConfig{
				Enabled: false,
			},
		},
	}

	destYAML := []byte(`
proxy:
  logging:
    enabled: true
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	if !merged.Proxy.Logging.Enabled {
		t.Fatalf("expected proxy.logging.enabled to be true")
	}
}

func TestMergeConfigs_ResponseHeaderTimeout(t *testing.T) {
	base := &Config{}

	dest := &Config{
		Proxy: ProxyConfig{
			ResponseHeaderTimeout: "10s",
		},
	}

	merged := mergeConfigs(base, dest, nil)
	if merged.Proxy.ResponseHeaderTimeout != "10s" {
		t.Fatalf("expected response_header_timeout to be 10s, got %s", merged.Proxy.ResponseHeaderTimeout)
	}
}

func TestMergeConfigs_ReplaceAliasesWithNewValues(t *testing.T) {
	base := &Config{
		Aliases: map[string]string{"deploy": "push", "logs": "tail"},
	}

	destYAML := []byte(`
aliases:
  build: compile
  run: exec
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	expected := map[string]string{"build": "compile", "run": "exec"}
	if !reflect.DeepEqual(merged.Aliases, expected) {
		t.Fatalf("expected aliases to be replaced with %v, got %v", expected, merged.Aliases)
	}
}

func TestMergeConfigs_ReplaceEnvClearWithNewValues(t *testing.T) {
	base := &Config{
		Env: EnvConfig{
			Clear: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
	}

	destYAML := []byte(`
env:
  clear:
    NEW_VAR: new_val
`)

	var dest Config
	if err := yaml.Unmarshal(destYAML, &dest); err != nil {
		t.Fatalf("failed to parse dest config: %v", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(destYAML, &node); err != nil {
		t.Fatalf("failed to parse dest YAML node: %v", err)
	}

	merged := mergeConfigs(base, &dest, &node)
	expected := map[string]string{"NEW_VAR": "new_val"}
	if !reflect.DeepEqual(merged.Env.Clear, expected) {
		t.Fatalf("expected env.clear to be replaced with %v, got %v", expected, merged.Env.Clear)
	}
}

func TestLoggingConfig_BackwardCompatUnmarshal(t *testing.T) {
	// Old field names should still work
	oldYAML := []byte(`
enabled: true
request_headers:
  - Authorization
  - Cookie
response_headers:
  - Set-Cookie
`)

	var cfg LoggingConfig
	if err := yaml.Unmarshal(oldYAML, &cfg); err != nil {
		t.Fatalf("failed to unmarshal old-style logging config: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled to be true")
	}
	if !reflect.DeepEqual(cfg.RedactRequestHeaders, []string{"Authorization", "Cookie"}) {
		t.Fatalf("expected old request_headers to map to RedactRequestHeaders, got %v", cfg.RedactRequestHeaders)
	}
	if !reflect.DeepEqual(cfg.RedactResponseHeaders, []string{"Set-Cookie"}) {
		t.Fatalf("expected old response_headers to map to RedactResponseHeaders, got %v", cfg.RedactResponseHeaders)
	}

	// New field names take precedence
	newYAML := []byte(`
redact_request_headers:
  - X-Api-Key
request_headers:
  - Authorization
`)

	var cfg2 LoggingConfig
	if err := yaml.Unmarshal(newYAML, &cfg2); err != nil {
		t.Fatalf("failed to unmarshal mixed logging config: %v", err)
	}
	if !reflect.DeepEqual(cfg2.RedactRequestHeaders, []string{"X-Api-Key"}) {
		t.Fatalf("expected new field to take precedence, got %v", cfg2.RedactRequestHeaders)
	}
}
