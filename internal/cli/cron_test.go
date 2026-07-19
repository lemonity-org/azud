package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
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
	for _, want := range []string{"crontab /tmp/crontab", "exec crond -f -l 2", "flock -n", "timeout 10m"} {
		if !strings.Contains(container.Command[1], want) {
			t.Fatalf("cron command missing %q: %s", want, container.Command[1])
		}
	}
	runCommand := container.BuildRunCommand()
	if !strings.Contains(runCommand, `-v ${HOME}/.local/share/azud:/var/lib/azud:rw`) {
		t.Fatalf("rootless lock mount will not expand remote HOME: %s", runCommand)
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
