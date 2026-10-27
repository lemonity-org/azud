package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/state"
)

func withCronTestConfig(t *testing.T, testConfig *config.Config) {
	t.Helper()
	previousConfig := cfg
	previousHost := cronHost
	cfg = testConfig
	cronHost = ""
	t.Cleanup(func() {
		cfg = previousConfig
		cronHost = previousHost
	})
}

func TestCronContainerUsesValidatedShellEntrypointAndRootlessLockPath(t *testing.T) {
	withCronTestConfig(t, &config.Config{
		Service: "shop",
		Image:   "ghcr.io/acme/shop:v1",
		SSH:     config.SSHConfig{User: "deploy"},
		Env:     config.EnvConfig{Clear: map[string]string{"RAILS_ENV": "production"}},
	})
	job := config.CronConfig{
		Schedule: "*/5 * * * *",
		Command:  "bin/cleanup --older-than '7 days'",
		Lock:     true,
		Timeout:  "10m",
	}

	container := buildCronContainerConfig("cleanup", job)
	if container.Entrypoint != "/bin/sh" || len(container.Command) != 2 || container.Command[0] != "-c" {
		t.Fatalf("cron shell contract = entrypoint %q command %v", container.Entrypoint, container.Command)
	}
	for _, want := range []string{"crontab /tmp/crontab", "exec crond -f -l 2", "flock -n /run/azud-cron-lock/cron.lock", "timeout 10m"} {
		if !strings.Contains(container.Command[1], want) {
			t.Fatalf("cron command missing %q: %s", want, container.Command[1])
		}
	}
	if strings.Contains(container.Command[1], "/var/lib/azud") {
		t.Fatalf("cron command still references the state directory: %s", container.Command[1])
	}
	runCommand := container.BuildRunCommand()
	if !strings.Contains(runCommand, `-v ${HOME}/'.local/share/azud-locks/cron/shop/cleanup:/run/azud-cron-lock:ro,Z'`) {
		t.Fatalf("rootless lock mount will not expand remote HOME or is not read-only: %s", runCommand)
	}
	if container.Labels[cronLockLabel] != cronLockLabelValue {
		t.Fatalf("cron container labels = %v, want %s=%s", container.Labels, cronLockLabel, cronLockLabelValue)
	}
}

func TestCronLockMountNeverExposesStateDir(t *testing.T) {
	for _, user := range []string{"root", "deploy"} {
		t.Run(user, func(t *testing.T) {
			withCronTestConfig(t, &config.Config{
				Service: "shop",
				Image:   "shop:v1",
				SSH:     config.SSHConfig{User: user},
				Volumes: []string{"/srv/uploads:/uploads"},
			})
			container := buildCronContainerConfig("backup", config.CronConfig{
				Schedule: "0 2 * * *",
				Command:  "bin/backup",
				Lock:     true,
			})
			lockDir := state.CronLockDir(user, "shop", "backup")
			want := []string{"/srv/uploads:/uploads", lockDir + ":/run/azud-cron-lock:ro,Z"}
			if !reflect.DeepEqual(container.Volumes, want) {
				t.Fatalf("volumes = %v, want %v", container.Volumes, want)
			}
			stateDir := state.Dir(user)
			for _, volume := range container.Volumes {
				source := strings.SplitN(volume, ":", 2)[0]
				if source == stateDir || strings.HasPrefix(source, stateDir+"/") {
					t.Fatalf("cron volume %q exposes state dir %q", volume, stateDir)
				}
			}
			// The host-side lock used by `cron run` must be the file the
			// container sees through its mount.
			if got, want := cronLockFile("backup"), lockDir+"/"+state.CronLockFileName; got != want {
				t.Fatalf("host lock file = %q, want %q", got, want)
			}
			if got := cronLockFileInContainer("backup"); got != "/run/azud-cron-lock/"+state.CronLockFileName {
				t.Fatalf("container lock file = %q", got)
			}
		})
	}
}

func TestCronWithoutLockMountsNoLockDir(t *testing.T) {
	withCronTestConfig(t, &config.Config{Service: "shop", Image: "shop:v1", SSH: config.SSHConfig{User: "root"}})
	container := buildCronContainerConfig("backup", config.CronConfig{Schedule: "0 2 * * *", Command: "bin/backup"})
	if len(container.Volumes) != 0 {
		t.Fatalf("cron without lock mounted %v", container.Volumes)
	}
	if _, ok := container.Labels[cronLockLabel]; ok {
		t.Fatalf("cron without lock has label %s", cronLockLabel)
	}
	if strings.Contains(container.Command[1], "flock") {
		t.Fatalf("cron without lock uses flock: %s", container.Command[1])
	}
}

func TestCronLockPrepareCommandRefusesSymlinksAndSetsModes(t *testing.T) {
	tests := []struct {
		name string
		user string
		want []string
	}{
		{
			name: "root",
			user: "root",
			want: []string{
				"root=/var/lib/azud-locks",
				"dir=/var/lib/azud-locks/cron/shop/backup",
				"lock=/var/lib/azud-locks/cron/shop/backup/cron.lock",
			},
		},
		{
			name: "rootless",
			user: "deploy",
			want: []string{
				`root="${HOME}/.local/share/azud-locks"`,
				`dir="${HOME}/".local/share/azud-locks/cron/shop/backup`,
				`lock="${HOME}/".local/share/azud-locks/cron/shop/backup/cron.lock`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := cronLockPrepareCommand(tt.user, "shop", "backup")
			if !strings.HasPrefix(command, "sh -c '") {
				t.Fatalf("prepare command is not a single sh -c script: %s", command)
			}
			for _, want := range append(tt.want,
				"set -eu",
				"umask 077",
				`if [ -L "$path" ]; then echo "refusing symlinked lock path $path" >&2; exit 1; fi`,
				`chmod 700 "$root" "$root/cron" "$root/cron/"shop`,
				`chmod 711 "$dir"`,
				`if [ -L "$lock" ]; then echo "refusing symlinked lock file $lock" >&2; exit 1; fi`,
				`chmod 644 "$lock"`,
			) {
				if !strings.Contains(command, want) {
					t.Fatalf("prepare command missing %q:\n%s", want, command)
				}
			}
			if strings.Contains(command, state.Dir(tt.user)+"/") || strings.Contains(command, "caddy") {
				t.Fatalf("prepare command touches the state directory:\n%s", command)
			}
		})
	}
}

func TestCronLockPrepareCommandCreatesReadableLockAndRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	run := func() error {
		cmd := exec.Command("sh", "-c", cronLockPrepareCommand("deploy", "shop", "backup"))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("prepare output: %s", out)
		}
		return err
	}
	if err := run(); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if err := run(); err != nil {
		t.Fatalf("prepare is not idempotent: %v", err)
	}
	locks := filepath.Join(home, ".local", "share", "azud-locks")
	for path, want := range map[string]os.FileMode{
		locks:                                0o700,
		filepath.Join(locks, "cron"):         0o700,
		filepath.Join(locks, "cron", "shop"): 0o700,
		filepath.Join(locks, "cron", "shop", "backup"):              0o711,
		filepath.Join(locks, "cron", "shop", "backup", "cron.lock"): 0o644,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s mode = %v, want %o", path, info.Mode(), want)
		}
	}

	// A symlinked job directory or lock file is refused, not followed.
	outside := t.TempDir()
	jobDir := filepath.Join(locks, "cron", "shop", "backup")
	if err := os.RemoveAll(jobDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, jobDir); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("prepare followed a symlinked job directory")
	}
	if err := os.Remove(jobDir); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatalf("prepare failed after removing the symlink: %v", err)
	}
	lockFile := filepath.Join(jobDir, "cron.lock")
	if err := os.Remove(lockFile); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "created-by-flock")
	if err := os.Symlink(victim, lockFile); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("prepare accepted a symlinked lock file")
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatalf("prepare created the symlink target: %v", err)
	}
}

func TestCronContainerRecreateDecision(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		lock    bool
		want    bool
		wantErr bool
	}{
		{
			name: "legacy state mount with lock",
			raw:  `[{"Config":{"Labels":{"azud.cron":"backup"}},"Mounts":[{"Type":"bind","Source":"/var/lib/azud","Destination":"/var/lib/azud","RW":true}]}]`,
			lock: true,
			want: true,
		},
		{
			name: "legacy state mount after lock was disabled",
			raw:  `[{"Config":{"Labels":{"azud.cron":"backup"}},"Mounts":[{"Destination":"/var/lib/azud/"}]}]`,
			want: true,
		},
		{
			name: "current lock mount",
			raw:  `[{"Config":{"Labels":{"azud.cron.lock":"job-dir"}},"Mounts":[{"Source":"/var/lib/azud-locks/cron/shop/backup","Destination":"/run/azud-cron-lock","RW":false}]}]`,
			lock: true,
		},
		{
			name: "lock enabled on a container without the lock label",
			raw:  `[{"Config":{"Labels":{"azud.cron":"backup"}},"Mounts":[]}]`,
			lock: true,
			want: true,
		},
		{
			name: "no lock and no legacy mount",
			raw:  `[{"Config":{"Labels":{"azud.cron":"backup"}},"Mounts":[{"Destination":"/uploads"}]}]`,
		},
		{
			name:    "empty inspect",
			raw:     `[]`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containerState, err := parseCronContainerInspect(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := containerState.needsRecreate(tt.lock); got != tt.want {
				t.Fatalf("needsRecreate(%v) = %v, want %v (state %+v)", tt.lock, got, tt.want, containerState)
			}
		})
	}
	if (cronContainerState{}).needsRecreate(true) {
		t.Fatal("a missing container must not be recreated")
	}
}

func TestLegacyCronLockCheckRefusesSymlink(t *testing.T) {
	withCronTestConfig(t, &config.Config{Service: "shop", SSH: config.SSHConfig{User: "deploy"}})
	if got, want := legacyCronLockCheckCommand("backup"), `test ! -L "${HOME}/.local/share/azud/shop-cron-backup.lock"`; got != want {
		t.Fatalf("legacy lock check = %q, want %q", got, want)
	}
	if got, want := legacyCronLockFile("backup"), "${HOME}/.local/share/azud/shop-cron-backup.lock"; got != want {
		t.Fatalf("legacy lock file = %q, want %q", got, want)
	}
}

func TestCronManualRunUsesSameShellAndTimeoutContract(t *testing.T) {
	withCronTestConfig(t, &config.Config{Service: "shop", Image: "shop:v1"})
	container := buildCronRunContainerConfig("cleanup", "shop-cron-cleanup-run", config.CronConfig{
		Command: "bin/cleanup --all",
		Timeout: "30s",
	})
	if container.Entrypoint != "/bin/sh" || !reflect.DeepEqual(container.Command[:1], []string{"-c"}) {
		t.Fatalf("manual shell contract = entrypoint %q command %v", container.Entrypoint, container.Command)
	}
	if !strings.Contains(container.Command[1], "timeout 30s sh -c") {
		t.Fatalf("manual timeout missing: %v", container.Command)
	}
}

func TestGetCronHostsRejectsUnconfiguredSelection(t *testing.T) {
	withCronTestConfig(t, &config.Config{Cron: map[string]config.CronConfig{
		"cleanup": {Hosts: []string{"one", "two"}},
	}})
	if got := getCronHosts("cleanup"); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("all hosts = %v", got)
	}
	cronHost = "other"
	if got := getCronHosts("cleanup"); got != nil {
		t.Fatalf("unconfigured selected host = %v", got)
	}
}
