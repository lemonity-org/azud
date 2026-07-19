package deploy

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

func roleTestConfig() *config.Config {
	return &config.Config{
		Service: "shop",
		Image:   "example/shop:latest",
		Servers: map[string]config.RoleConfig{
			"web": {
				Hosts: []string{"shared", "web-only"},
				Env:   map[string]string{"ROLE_ENV": "web"},
			},
			"worker": {
				Hosts:   []string{"shared", "worker-only"},
				Cmd:     "bundle exec jobs",
				Env:     map[string]string{"ROLE_ENV": "worker"},
				Labels:  map[string]string{"team": "jobs", "azud.managed": "false", "azud.role": "spoofed"},
				Options: map[string]string{"memory": "512M", "cpus": "0.5"},
			},
		},
		Env: config.EnvConfig{Clear: map[string]string{"GLOBAL": "yes", "ROLE_ENV": "global"}},
		Proxy: config.ProxyConfig{
			AppPort: 3000,
			Rootful: true,
			Healthcheck: config.HealthcheckConfig{
				LivenessPath: "/live",
			},
		},
		Podman: config.PodmanConfig{Rootless: true},
	}
}

func TestNewAppContainerConfigAppliesRoleSemantics(t *testing.T) {
	cfg := roleTestConfig()
	worker := NewAppContainerConfig(cfg, cfg.Image, "shop-worker-new", "worker", map[string]string{
		"azud.service":  "spoofed",
		"azud.instance": "2",
	})

	if worker.Labels["azud.managed"] != "true" || worker.Labels["azud.service"] != "shop" || worker.Labels["azud.role"] != "worker" {
		t.Fatalf("managed role labels can be spoofed: %#v", worker.Labels)
	}
	if worker.Labels["team"] != "jobs" || worker.Labels["azud.instance"] != "2" {
		t.Fatalf("role and caller labels were not preserved: %#v", worker.Labels)
	}
	if worker.Env["GLOBAL"] != "yes" || worker.Env["ROLE_ENV"] != "worker" {
		t.Fatalf("role environment did not override global environment: %#v", worker.Env)
	}
	if !reflect.DeepEqual(worker.Command, []string{"bundle", "exec", "jobs"}) {
		t.Fatalf("worker command = %#v", worker.Command)
	}
	if worker.Memory != "512M" || worker.CPUs != "0.5" {
		t.Fatalf("worker resources = memory %q cpus %q", worker.Memory, worker.CPUs)
	}
	if len(worker.Ports) != 0 || worker.HealthCmd != "" {
		t.Fatalf("non-web role inherited HTTP behavior: ports=%v health=%q", worker.Ports, worker.HealthCmd)
	}
	if got := worker.NetworkAliases; !reflect.DeepEqual(got, []string{"shop-worker"}) {
		t.Fatalf("worker aliases = %v", got)
	}

	web := NewAppContainerConfig(cfg, cfg.Image, "shop-new", "web", nil)
	if !reflect.DeepEqual(web.Ports, []string{"127.0.0.1::3000"}) {
		t.Fatalf("web host ports = %v", web.Ports)
	}
	if web.HealthCmd == "" {
		t.Fatal("web role should have a liveness health command")
	}
	if got := RoleContainerName(cfg, "web"); got != "shop" {
		t.Fatalf("web stable name = %q", got)
	}
	if got := RoleContainerName(cfg, "worker"); got != "shop-worker" {
		t.Fatalf("worker stable name = %q", got)
	}
}

func TestGetTargetsPreservesRoleIdentityAndOrdering(t *testing.T) {
	d := &Deployer{cfg: roleTestConfig()}
	targets, err := d.getTargets(&DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []deploymentTarget{
		{Host: "shared", Role: "web"},
		{Host: "web-only", Role: "web"},
		{Host: "shared", Role: "worker"},
		{Host: "worker-only", Role: "worker"},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets = %#v, want %#v", targets, want)
	}
	if got := targetHosts(targets); !reflect.DeepEqual(got, []string{"shared", "web-only", "worker-only"}) {
		t.Fatalf("unique hosts = %v", got)
	}

	filtered, err := d.getTargets(&DeployOptions{Roles: []string{"worker"}, Hosts: []string{"shared"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filtered, []deploymentTarget{{Host: "shared", Role: "worker"}}) {
		t.Fatalf("filtered targets = %#v", filtered)
	}
}

func TestGetTargetsRejectsUnknownSelections(t *testing.T) {
	d := &Deployer{cfg: roleTestConfig()}
	if _, err := d.getTargets(&DeployOptions{Roles: []string{"missing"}}); err == nil {
		t.Fatal("expected unknown role to fail")
	}
	if _, err := d.getTargets(&DeployOptions{Roles: []string{"worker"}, Hosts: []string{"web-only"}}); err == nil {
		t.Fatal("expected a host outside the selected role to fail")
	}
}

func TestParseCommandArgsPreservesQuotedAndShellCommands(t *testing.T) {
	if got := ParseCommandArgs("redis-server --appendonly yes"); !reflect.DeepEqual(got, []string{"redis-server", "--appendonly", "yes"}) {
		t.Fatalf("plain command = %#v", got)
	}
	quoted := `worker --queue "high priority"`
	if got := ParseCommandArgs(quoted); !reflect.DeepEqual(got, []string{"sh", "-c", quoted}) {
		t.Fatalf("quoted command = %#v", got)
	}
	compound := `worker --once && echo done`
	if got := ParseCommandArgs(compound); !reflect.DeepEqual(got, []string{"sh", "-c", compound}) {
		t.Fatalf("compound command = %#v", got)
	}
}

func TestFleetFailureStopsSchedulingAndRollsBackEverySuccess(t *testing.T) {
	targets := []deploymentTarget{
		{Host: "one", Role: "web"},
		{Host: "two", Role: "web"},
		{Host: "three", Role: "web"},
	}
	d := &Deployer{log: output.DefaultLogger}
	var attempted []string
	var rolledBack []deploymentTarget
	_, failures := d.runFleetDeployment(
		targets,
		true,
		func(target deploymentTarget) error {
			attempted = append(attempted, target.Host)
			if target.Host == "two" {
				return errors.New("injected host failure")
			}
			return nil
		},
		func(targets []deploymentTarget) error {
			rolledBack = append(rolledBack, targets...)
			return errors.New("injected rollback failure")
		},
	)
	if !reflect.DeepEqual(attempted, []string{"one", "two"}) {
		t.Fatalf("scheduled targets = %v; host three must not start", attempted)
	}
	if !reflect.DeepEqual(rolledBack, []deploymentTarget{{Host: "one", Role: "web"}}) {
		t.Fatalf("rollback targets = %#v", rolledBack)
	}
	if len(failures) != 2 || !strings.Contains(failures[1], "automatic rollback") {
		t.Fatalf("rollback failure was not reported: %v", failures)
	}
}
