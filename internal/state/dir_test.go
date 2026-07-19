package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDirRequiresAbsoluteOverride(t *testing.T) {
	t.Setenv("AZUD_STATE_DIR", "relative/state")
	if _, err := LocalDir(); err == nil {
		t.Fatal("expected relative AZUD_STATE_DIR to fail")
	}
}

func TestEnsureLocalDirCreatesPrivateOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("AZUD_STATE_DIR", dir)
	got, err := EnsureLocalDir()
	if err != nil {
		t.Fatalf("EnsureLocalDir: %v", err)
	}
	if got != dir {
		t.Fatalf("state dir = %q, want %q", got, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("state directory mode = %o, want 700", info.Mode().Perm())
	}
}
