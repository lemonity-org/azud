package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestCheckLocalSecretsFileMode(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	tests := []struct {
		name     string
		provider string
		mode     os.FileMode
		missing  bool
		wantErr  bool
	}{
		{name: "private file", mode: 0o600},
		{name: "read-only owner file", mode: 0o400},
		{name: "group readable", mode: 0o640, wantErr: true},
		{name: "world readable", mode: 0o644, wantErr: true},
		{name: "world writable", mode: 0o602, wantErr: true},
		{name: "explicit file provider", provider: "file", mode: 0o644, wantErr: true},
		{name: "missing file", missing: true},
		{name: "env provider ignores file", provider: "env", mode: 0o644},
		{name: "command provider ignores file", provider: "command", mode: 0o644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
			if !tt.missing {
				writeSecretsFixture(t, secretsPath, "DB_PASS=hunter2\n", tt.mode)
			}
			previousConfig := cfg
			cfg = &config.Config{SecretsPath: secretsPath, SecretsProvider: tt.provider}
			t.Cleanup(func() { cfg = previousConfig })

			err := checkLocalSecretsFileMode()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkLocalSecretsFileMode: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an insecure secrets file to fail preflight")
			}
			for _, want := range []string{"readable by group or others", "chmod 600 " + secretsPath} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
