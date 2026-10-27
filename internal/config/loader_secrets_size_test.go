package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/secretsfile"
)

func TestLoadSecretsFromFileReadsLongestWritableValue(t *testing.T) {
	t.Cleanup(func() { SetLoadedSecrets(nil) })
	long := strings.Repeat("x", secretsfile.MaxValueBytes)
	content, err := secretsfile.Format("# Azud Secrets\n\n", map[string]string{"BIG": long, "OTHER": "other"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	path := filepath.Join(t.TempDir(), "secrets")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{SecretsPath: path}
	if err := (&Loader{}).loadSecretsFromFile(cfg); err != nil {
		t.Fatalf("loadSecretsFromFile: %v", err)
	}
	if cfg.loadedSecrets["BIG"] != long || cfg.loadedSecrets["OTHER"] != "other" {
		t.Fatalf("secrets did not round-trip: BIG has %d bytes, OTHER = %q", len(cfg.loadedSecrets["BIG"]), cfg.loadedSecrets["OTHER"])
	}
}
