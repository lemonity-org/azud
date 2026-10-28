package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadSecretsFromFileWarnsButLoadsInsecureMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not available on Windows")
	}
	t.Cleanup(func() { SetLoadedSecrets(nil) })

	tests := []struct {
		name     string
		mode     os.FileMode
		wantWarn bool
	}{
		{name: "private", mode: 0o600},
		{name: "group readable", mode: 0o640, wantWarn: true},
		{name: "world readable", mode: 0o644, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets")
			if err := os.WriteFile(path, []byte("DB_PASS=hunter2\n"), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}

			cfg := &Config{SecretsPath: path}
			stderr := captureStderr(t, func() {
				if err := (&Loader{}).loadSecretsFromFile(cfg); err != nil {
					t.Fatalf("loadSecretsFromFile: %v", err)
				}
			})

			if cfg.loadedSecrets["DB_PASS"] != "hunter2" {
				t.Fatalf("secrets were not loaded: %#v", cfg.loadedSecrets)
			}
			warned := strings.Contains(stderr, "insecure permissions")
			if warned != tt.wantWarn {
				t.Fatalf("warning emitted = %v, want %v; stderr:\n%s", warned, tt.wantWarn, stderr)
			}
			if tt.wantWarn && !strings.Contains(stderr, "chmod 600 "+path) {
				t.Fatalf("warning does not tell the user how to fix the mode:\n%s", stderr)
			}
		})
	}
}

func TestLoadSecretsFromFileRejectsRawDotDot(t *testing.T) {
	t.Cleanup(func() { SetLoadedSecrets(nil) })

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app", "x", "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "x", "prod.env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("x", "y"), filepath.Join(dir, "app", "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Cleaned, this is app/prod.env, but the OS opens app/x/prod.env through
	// the symlink, which build exclusions computed from the cleaned path miss.
	path := filepath.Join(dir, "app", "sub") + "/../prod.env"
	err := (&Loader{}).loadSecretsFromFile(&Config{SecretsPath: path})
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("loadSecretsFromFile(%q) error = %v, want path traversal error", path, err)
	}
}
