package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDestination(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"staging", true},
		{"production", true},
		{"eu-west-1", true},
		{"prod_2", true},
		{"eu.prod", true},
		{"2026", true},
		{strings.Repeat("a", 63), true},

		{"", false},
		{"/../../victim", false},
		{"../victim", false},
		{"a/b", false},
		{`a\b`, false},
		{"..", false},
		{"a..b", false},
		{".", false},
		{".hidden", false},
		{"_staging", false},
		{"-x", false},
		{" staging", false},
		{"staging\n", false},
		{"stag ing", false},
		{"staging;id", false},
		{strings.Repeat("a", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDestination(tt.name)
			if tt.valid && err != nil {
				t.Fatalf("ValidateDestination(%q) error: %v", tt.name, err)
			}
			if !tt.valid {
				if err == nil {
					t.Fatalf("ValidateDestination(%q) accepted an unsafe name", tt.name)
				}
				if !strings.Contains(err.Error(), "invalid destination") {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// writeDestinationFixture writes config/deploy.yml under a temporary
// directory and returns the base config path.
func writeDestinationFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(path, []byte(envTestBaseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoaderRejectsUnsafeDestination(t *testing.T) {
	path := writeDestinationFixture(t)
	// filepath.Join("config", "deploy./../../victim.yml") resolves to a file
	// outside the config directory; make sure it exists.
	victim := filepath.Join(filepath.Dir(filepath.Dir(path)), "victim.yml")
	if err := os.WriteFile(victim, []byte("service: attacker-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewLoader(path, "/../../victim").Load()
	if err == nil {
		t.Fatalf("unsafe destination loaded service %q", cfg.Service)
	}
	if !strings.Contains(err.Error(), "invalid destination") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoaderMergesDestinationOverride(t *testing.T) {
	path := writeDestinationFixture(t)
	destPath := filepath.Join(filepath.Dir(path), "deploy.eu.prod.yml")
	if err := os.WriteFile(destPath, []byte("service: test-eu\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewLoader(path, "eu.prod").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Service != "test-eu" {
		t.Fatalf("service = %q, want the destination override", cfg.Service)
	}
}

func TestLoaderDestinationWithoutOverrideUsesBase(t *testing.T) {
	path := writeDestinationFixture(t)

	cfg, err := NewLoader(path, "staging").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Service != "test" {
		t.Fatalf("service = %q, want the base value", cfg.Service)
	}
}

func TestLoaderDestinationStatErrorFails(t *testing.T) {
	path := writeDestinationFixture(t)
	destPath := filepath.Join(filepath.Dir(path), "deploy.staging.yml")
	// A self-referencing symlink makes stat fail with an error other than
	// "not exist", which must not fall back to the base configuration.
	if err := os.Symlink(filepath.Base(destPath), destPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := NewLoader(path, "staging").Load()
	if err == nil {
		t.Fatal("expected an unreadable destination config to fail")
	}
	if !strings.Contains(err.Error(), "failed to load destination config") {
		t.Fatalf("unexpected error: %v", err)
	}
}
