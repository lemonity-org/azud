package proxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runStateCommand runs a generated state-directory command with /bin/sh the
// way a remote login shell would, using home as ${HOME}.
func runStateCommand(t *testing.T, home, command, stdin string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("command output: %s", out)
	}
	return err
}

func TestPersistConfigCommandDoesNotFollowPlantedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both the old fixed temp name and the final path point at the victim.
	for _, name := range []string{CaddyConfigFileName + ".tmp", CaddyConfigFileName} {
		if err := os.Symlink(victim, filepath.Join(stateDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if err := runStateCommand(t, home, persistConfigCommand("deploy"), `{"apps":{}}`); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" || info.Mode().Perm() != 0o644 {
		t.Fatalf("persist wrote through a symlink: victim = %q mode %o", data, info.Mode().Perm())
	}
	configFile := filepath.Join(stateDir, CaddyConfigFileName)
	linfo, err := os.Lstat(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !linfo.Mode().IsRegular() || linfo.Mode().Perm() != 0o600 {
		t.Fatalf("persisted config mode = %v, want regular 0600", linfo.Mode())
	}
	if data, _ := os.ReadFile(configFile); string(data) != `{"apps":{}}` {
		t.Fatalf("persisted config = %q", data)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+CaddyConfigFileName+".") {
			t.Fatalf("persist left temp file %s behind", entry.Name())
		}
	}
}

func TestRestoreConfigCommandRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte(`{"secret":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(stateDir, CaddyConfigFileName)); err != nil {
		t.Fatal(err)
	}

	if err := runStateCommand(t, home, restoreConfigCommand("deploy"), ""); err == nil {
		t.Fatal("restore accepted a symlinked config file")
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("restore changed the symlink target's mode to %o", info.Mode().Perm())
	}
}

// The migration rollback state holds a raw Caddy autosave, which may carry
// private keys, so it gets the persisted config's protections.
func TestProxyRollbackStateIsAProtectedStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, proxyRollbackFileName)
	if err := os.Symlink(victim, stateFile); err != nil {
		t.Fatal(err)
	}
	if err := runStateCommand(t, home, readStateFileCommand("deploy", proxyRollbackFileName), ""); err == nil {
		t.Fatal("read accepted a symlinked rollback state file")
	}

	saved := `{"container":"abc","running":true,"snapshotted":true,"autosave":"{\"admin\":{\"listen\":\"0.0.0.0:2019\"}}"}`
	if err := runStateCommand(t, home, writeStateFileCommand("deploy", proxyRollbackFileName), saved); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "original" {
		t.Fatalf("write followed a symlink: victim = %q", data)
	}
	info, err := os.Lstat(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("rollback state mode = %v in a %v directory, want a regular 0600 file in 0700", info.Mode(), dirInfo.Mode().Perm())
	}
	cmd := exec.Command("sh", "-c", readStateFileCommand("deploy", proxyRollbackFileName))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil || string(out) != saved {
		t.Fatalf("read = %q, %v; want %q", out, err, saved)
	}
}

func TestCopyManagedProxySettingsRemovesLogCredentials(t *testing.T) {
	newConfig := func(server map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"apps": map[string]interface{}{
				"http": map[string]interface{}{
					"servers": map[string]interface{}{"srv0": server},
				},
			},
		}
	}
	for _, loggingEnabled := range []bool{true, false} {
		current := newConfig(map[string]interface{}{
			"listen": []interface{}{":80"},
			"logs": map[string]interface{}{
				"default_logger_name":    "access",
				"should_log_credentials": true,
			},
		})
		manager := &Manager{}
		desiredConfig := manager.buildBaseConfig()
		manager.applyProxySettingsFrom(desiredConfig, &ProxyConfig{LoggingEnabled: loggingEnabled})
		data, err := json.Marshal(desiredConfig)
		if err != nil {
			t.Fatal(err)
		}
		var desired map[string]interface{}
		if err := json.Unmarshal(data, &desired); err != nil {
			t.Fatal(err)
		}

		if err := copyManagedProxySettings(current, desired); err != nil {
			t.Fatal(err)
		}
		server := current["apps"].(map[string]interface{})["http"].(map[string]interface{})["servers"].(map[string]interface{})["srv0"].(map[string]interface{})
		logs, hasLogs := server["logs"].(map[string]interface{})
		if hasLogs != loggingEnabled {
			t.Fatalf("logging enabled=%v but reconciled logs=%#v", loggingEnabled, server["logs"])
		}
		if _, ok := logs["should_log_credentials"]; ok {
			t.Fatalf("should_log_credentials survived reconciliation (logging enabled=%v): %#v", loggingEnabled, server)
		}
		if strings.Contains(string(data), "log_credentials") {
			t.Fatalf("desired Caddy config enables credential logging: %s", data)
		}
	}
}

func TestSystemdUnitFromInspect(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "direct podman container",
			raw:  `[{"Config":{"Labels":{"azud.managed":"true","azud.type":"proxy"}}}]`,
		},
		{
			name: "quadlet container",
			raw:  `[{"Config":{"Labels":{"azud.type":"proxy","PODMAN_SYSTEMD_UNIT":"azud-proxy.service"}}}]`,
			want: "azud-proxy.service",
		},
		{
			name: "no labels",
			raw:  `[{"Config":{}}]`,
		},
		{
			name:    "empty result",
			raw:     `[]`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			raw:     ``,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := systemdUnitFromInspect(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got unit %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemctlStopCommandMatchesProxyRuntime(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		rootful bool
		want    string
	}{
		{name: "root", user: "root", want: "systemctl stop azud-proxy.service"},
		{name: "root rootful", user: "root", rootful: true, want: "systemctl stop azud-proxy.service"},
		{name: "rootful via sudo", user: "deploy", rootful: true, want: "sudo systemctl stop azud-proxy.service"},
		{name: "rootless", user: "deploy", want: "systemctl --user stop azud-proxy.service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{user: tt.user, rootful: tt.rootful}
			if got := manager.systemctlStopCommand("azud-proxy.service"); got != tt.want {
				t.Fatalf("systemctl command = %q, want %q", got, tt.want)
			}
		})
	}
}
