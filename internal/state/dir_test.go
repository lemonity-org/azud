package state

import (
	"strings"
	"testing"
)

func TestLocalDirRequiresAbsoluteOverride(t *testing.T) {
	t.Setenv("AZUD_STATE_DIR", "relative/state")
	if _, err := LocalDir(); err == nil {
		t.Fatal("expected relative AZUD_STATE_DIR to fail")
	}
}

func TestCronLockPathsStayOutsideStateDir(t *testing.T) {
	tests := []struct {
		name       string
		user       string
		dir        string
		file       string
		dirQuoted  string
		fileQuoted string
		locks      string
	}{
		{
			name:       "root",
			user:       "root",
			dir:        "/var/lib/azud-locks/cron/shop/backup",
			file:       "/var/lib/azud-locks/cron/shop/backup/cron.lock",
			dirQuoted:  "/var/lib/azud-locks/cron/shop/backup",
			fileQuoted: "/var/lib/azud-locks/cron/shop/backup/cron.lock",
			locks:      "/var/lib/azud-locks",
		},
		{
			name:       "empty user is root",
			user:       "",
			dir:        "/var/lib/azud-locks/cron/shop/backup",
			file:       "/var/lib/azud-locks/cron/shop/backup/cron.lock",
			dirQuoted:  "/var/lib/azud-locks/cron/shop/backup",
			fileQuoted: "/var/lib/azud-locks/cron/shop/backup/cron.lock",
			locks:      "/var/lib/azud-locks",
		},
		{
			name:       "rootless",
			user:       "deploy",
			dir:        "${HOME}/.local/share/azud-locks/cron/shop/backup",
			file:       "${HOME}/.local/share/azud-locks/cron/shop/backup/cron.lock",
			dirQuoted:  `"${HOME}/".local/share/azud-locks/cron/shop/backup`,
			fileQuoted: `"${HOME}/".local/share/azud-locks/cron/shop/backup/cron.lock`,
			locks:      `"${HOME}/.local/share/azud-locks"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CronLockDir(tt.user, "shop", "backup"); got != tt.dir {
				t.Fatalf("CronLockDir = %q, want %q", got, tt.dir)
			}
			if got := CronLockFile(tt.user, "shop", "backup"); got != tt.file {
				t.Fatalf("CronLockFile = %q, want %q", got, tt.file)
			}
			if got := CronLockDirQuoted(tt.user, "shop", "backup"); got != tt.dirQuoted {
				t.Fatalf("CronLockDirQuoted = %q, want %q", got, tt.dirQuoted)
			}
			if got := CronLockFileQuoted(tt.user, "shop", "backup"); got != tt.fileQuoted {
				t.Fatalf("CronLockFileQuoted = %q, want %q", got, tt.fileQuoted)
			}
			if got := LocksDirQuoted(tt.user); got != tt.locks {
				t.Fatalf("LocksDirQuoted = %q, want %q", got, tt.locks)
			}
			stateDir := Dir(tt.user)
			if strings.HasPrefix(tt.dir+"/", stateDir+"/") {
				t.Fatalf("cron lock dir %q is inside state dir %q", tt.dir, stateDir)
			}
		})
	}
}

func TestCronLockQuotedPathsKeepNamesLiteral(t *testing.T) {
	got := CronLockDirQuoted("deploy", "shop", "a b$(id)")
	want := `"${HOME}/"'.local/share/azud-locks/cron/shop/a b$(id)'`
	if got != want {
		t.Fatalf("CronLockDirQuoted = %q, want %q", got, want)
	}
	got = CronLockFileQuoted("root", "shop", "a b")
	want = `'/var/lib/azud-locks/cron/shop/a b/cron.lock'`
	if got != want {
		t.Fatalf("CronLockFileQuoted = %q, want %q", got, want)
	}
}
