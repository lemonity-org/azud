package config

import (
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
