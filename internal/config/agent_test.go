package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// verifyImage is a verify_command as it reads after loading, with $$ unescaped.
const verifyImage = `cosign verify "$AZUD_AGENT_IMAGE"`

func agentTestConfig(agent AgentConfig) *Config {
	return &Config{
		Service: "app",
		Image:   "ghcr.io/lemonity-org/app",
		Servers: map[string]RoleConfig{
			"web":    {Hosts: []string{"app-1"}},
			"worker": {Hosts: []string{"app-1"}},
		},
		Proxy: ProxyConfig{Host: "app.example.com"},
		SSH:   SSHConfig{Port: 22},
		Agent: agent,
	}
}

func TestValidateAgent(t *testing.T) {
	tests := []struct {
		name    string
		agent   AgentConfig
		wantErr string
	}{
		{name: "no agent section", agent: AgentConfig{}},
		{
			name:  "verified pull mode",
			agent: AgentConfig{Track: "production", VerifyCommand: `cosign verify "$AZUD_AGENT_IMAGE"`, Interval: time.Minute},
		},
		{name: "explicitly unverified", agent: AgentConfig{Track: "production", InsecureSkipVerify: true}},
		{name: "track required", agent: AgentConfig{VerifyCommand: verifyImage}, wantErr: "agent.track"},
		{name: "invalid track", agent: AgentConfig{Track: "-prod", VerifyCommand: verifyImage}, wantErr: "agent.track"},
		{name: "verification required", agent: AgentConfig{Track: "production"}, wantErr: "agent.verify_command"},
		{
			name:    "verify and skip are exclusive",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, InsecureSkipVerify: true},
			wantErr: "agent.insecure_skip_verify",
		},
		{
			name:    "verify command must name the image",
			agent:   AgentConfig{Track: "production", VerifyCommand: `cosign verify --key cosign.pub ""`},
			wantErr: "$$AZUD_AGENT_IMAGE",
		},
		{
			name:    "interval floor",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, Interval: time.Second},
			wantErr: "agent.interval",
		},
		{
			name:  "custom deploy command",
			agent: AgentConfig{Track: "production", VerifyCommand: verifyImage, DeployCommand: `./deploy "$AZUD_AGENT_IMAGE"`, DeployTimeout: time.Hour},
		},
		{
			name:    "deploy command must name the image",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, DeployCommand: `./deploy ""`},
			wantErr: "agent.deploy_command",
		},
		{
			name:    "deploy timeout needs a deploy command",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, DeployTimeout: time.Hour},
			wantErr: "agent.deploy_timeout",
		},
		{
			name:    "negative deploy timeout",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, DeployCommand: `./deploy "$AZUD_AGENT_IMAGE"`, DeployTimeout: -time.Second},
			wantErr: "agent.deploy_timeout",
		},
		{
			name:    "no deploy command for bundles",
			agent:   AgentConfig{Track: "production", VerifyCommand: verifyImage, Source: AgentSourceBundle, DeployCommand: `./deploy "$AZUD_AGENT_IMAGE"`},
			wantErr: "not supported with source: bundle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(agentTestConfig(tt.agent))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want no error", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error on %s", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAgentRequiresSingleServer(t *testing.T) {
	cfg := agentTestConfig(AgentConfig{Track: "production", VerifyCommand: verifyImage})
	cfg.Servers["worker"] = RoleConfig{Hosts: []string{"app-2"}}

	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "one server") {
		t.Fatalf("Validate() = %v, want the single-server restriction", err)
	}

	// The restriction applies to pull mode only.
	cfg.Agent = AgentConfig{}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() without agent = %v", err)
	}
}

func TestLoadKeepsEscapedAgentImageVariable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yml")
	data := []byte(`service: app
image: ghcr.io/lemonity-org/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
agent:
  track: production
  verify_command: >-
    cosign verify
    --certificate-identity https://github.com/acme/app/.github/workflows/release.yml@refs/heads/main
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
    "$$AZUD_AGENT_IMAGE"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := NewLoader(path, "").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := `cosign verify --certificate-identity https://github.com/acme/app/.github/workflows/release.yml@refs/heads/main ` +
		`--certificate-oidc-issuer https://token.actions.githubusercontent.com "$AZUD_AGENT_IMAGE"`
	if cfg.Agent.VerifyCommand != want {
		t.Fatalf("verify_command = %q, want the variable kept for the agent to set", cfg.Agent.VerifyCommand)
	}
}

func TestAgentDefaultsApplyOnlyWhenTracking(t *testing.T) {
	cfg := agentTestConfig(AgentConfig{})
	applyDefaults(cfg)
	if cfg.Agent.Interval != 0 || cfg.Agent.VerifyTimeout != 0 {
		t.Fatalf("agent defaults applied without agent.track: %+v", cfg.Agent)
	}

	cfg = agentTestConfig(AgentConfig{Track: "production"})
	applyDefaults(cfg)
	if cfg.Agent.Interval != time.Minute || cfg.Agent.VerifyTimeout != 2*time.Minute {
		t.Fatalf("agent defaults = %+v, want 1m interval and 2m verify timeout", cfg.Agent)
	}
}

func TestDeployTimeoutDefaultsOnlyForADeployCommand(t *testing.T) {
	cfg := agentTestConfig(AgentConfig{Track: "production"})
	applyDefaults(cfg)
	if cfg.Agent.DeployTimeout != 0 {
		t.Fatalf("deploy_timeout = %s without deploy_command", cfg.Agent.DeployTimeout)
	}
	cfg = agentTestConfig(AgentConfig{Track: "production", DeployCommand: `./deploy "$AZUD_AGENT_IMAGE"`})
	applyDefaults(cfg)
	if cfg.Agent.DeployTimeout != AgentDefaultDeployTimeout {
		t.Fatalf("deploy_timeout = %s, want %s", cfg.Agent.DeployTimeout, AgentDefaultDeployTimeout)
	}
}

func TestMergeConfigs_AgentDeployCommand(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(base, []byte(`service: app
image: ghcr.io/lemonity-org/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.agent.yml"), []byte(`agent:
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
  deploy_command: scripts/release "$$AZUD_AGENT_IMAGE"
  deploy_timeout: 45m
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewLoader(base, "agent").Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.DeployCommand != `scripts/release "$AZUD_AGENT_IMAGE"` || cfg.Agent.DeployTimeout != 45*time.Minute {
		t.Fatalf("agent = %+v; want the destination's deploy_command and deploy_timeout", cfg.Agent)
	}
}

func TestMergeConfigs_Agent(t *testing.T) {
	base := &Config{Agent: AgentConfig{
		Track:              "staging",
		Interval:           2 * time.Minute,
		InsecureSkipVerify: true,
	}}

	destYAML := []byte(`
agent:
  track: production
  interval: 30s
  verify_command: cosign verify "$AZUD_AGENT_IMAGE"
  insecure_skip_verify: false
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
	want := AgentConfig{
		Track:         "production",
		Interval:      30 * time.Second,
		VerifyCommand: `cosign verify "$AZUD_AGENT_IMAGE"`,
	}
	if merged.Agent != want {
		t.Fatalf("merged agent = %+v, want %+v", merged.Agent, want)
	}
}
