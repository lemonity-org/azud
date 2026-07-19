package cli

import (
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestStripImageReference(t *testing.T) {
	tests := map[string]string{
		"ghcr.io/acme/app":                    "ghcr.io/acme/app",
		"ghcr.io/acme/app:old":                "ghcr.io/acme/app",
		"localhost:5000/acme/app:old":         "localhost:5000/acme/app",
		"localhost:5000/acme/app@sha256:abcd": "localhost:5000/acme/app",
	}
	for input, want := range tests {
		if got := stripImageReference(input); got != want {
			t.Errorf("stripImageReference(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGenerateImageTagNormalizesBaseAndValidatesTag(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = &config.Config{Builder: config.BuilderConfig{TagTemplate: "release-{destination}"}}

	got, err := generateImageTag("localhost:5000/acme/app:old", "production")
	if err != nil {
		t.Fatal(err)
	}
	if got != "localhost:5000/acme/app:release-production" {
		t.Fatalf("generated tag = %q", got)
	}

	cfg.Builder.TagTemplate = "invalid tag"
	if _, err := generateImageTag("acme/app@sha256:abcd", ""); err == nil || !strings.Contains(err.Error(), "valid OCI tag") {
		t.Fatalf("expected invalid tag error, got %v", err)
	}
}
