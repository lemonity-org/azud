package deploy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
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
				Runtime: config.RoleRuntimeConfig{
					User:            "10001:10001",
					ReadOnly:        true,
					CapDrop:         []string{"ALL"},
					NoNewPrivileges: true,
					Tmpfs: []config.RoleTmpfsConfig{{
						Path: "/tmp",
						Size: "64m",
						Mode: "1777",
					}},
					DisableHealthcheck: true,
					StopTimeout:        30 * time.Second,
				},
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
	if worker.User != "10001:10001" || !worker.ReadOnly || !worker.NoNewPrivileges || !worker.NoHealthcheck || worker.StopTimeout != 30 {
		t.Fatalf("worker runtime hardening was not mapped: %#v", worker)
	}
	if !reflect.DeepEqual(worker.CapDrop, []string{"ALL"}) ||
		!reflect.DeepEqual(worker.Tmpfs, []string{"/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777"}) {
		t.Fatalf("worker runtime lists were not mapped: caps=%v tmpfs=%v", worker.CapDrop, worker.Tmpfs)
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

type fakeContainerLifecycle struct {
	exists     map[string]bool
	running    map[string]bool
	operations []string
	failRun    bool
	failWait   bool
	failBefore map[string]bool
	failAfter  map[string]bool
	maxRunning int
}

func newFakeContainerLifecycle(runningNames ...string) *fakeContainerLifecycle {
	fake := &fakeContainerLifecycle{
		exists:     make(map[string]bool),
		running:    make(map[string]bool),
		failBefore: make(map[string]bool),
		failAfter:  make(map[string]bool),
	}
	for _, name := range runningNames {
		fake.exists[name] = true
		fake.running[name] = true
	}
	fake.captureMaxRunning()
	return fake
}

func (f *fakeContainerLifecycle) record(operation string) {
	f.operations = append(f.operations, operation)
}

func (f *fakeContainerLifecycle) injects(failures map[string]bool, operation string) bool {
	for prefix := range failures {
		if strings.HasPrefix(operation, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeContainerLifecycle) captureMaxRunning() {
	count := 0
	for _, running := range f.running {
		if running {
			count++
		}
	}
	if count > f.maxRunning {
		f.maxRunning = count
	}
}

func (f *fakeContainerLifecycle) Create(_ string, cfg *podman.ContainerConfig) (string, error) {
	operation := "create " + cfg.Name
	f.record(operation)
	if f.injects(f.failBefore, operation) {
		return "", errors.New("injected create failure")
	}
	f.exists[cfg.Name] = true
	if f.injects(f.failAfter, operation) {
		return "", errors.New("injected create response failure")
	}
	return cfg.Name, nil
}

func (f *fakeContainerLifecycle) Run(_ string, cfg *podman.ContainerConfig) (string, error) {
	operation := "run " + cfg.Name
	f.record(operation)
	if f.failRun || f.injects(f.failBefore, operation) {
		return "", errors.New("injected run failure")
	}
	f.exists[cfg.Name] = true
	f.running[cfg.Name] = true
	f.captureMaxRunning()
	if f.injects(f.failAfter, operation) {
		return "", errors.New("injected run response failure")
	}
	return cfg.Name, nil
}

func (f *fakeContainerLifecycle) Start(_ string, container string) error {
	operation := "start " + container
	f.record(operation)
	if f.injects(f.failBefore, operation) {
		return errors.New("injected start failure")
	}
	if !f.exists[container] {
		return fmt.Errorf("missing container %s", container)
	}
	f.running[container] = true
	f.captureMaxRunning()
	if f.injects(f.failAfter, operation) {
		return errors.New("injected start response failure")
	}
	return nil
}

func (f *fakeContainerLifecycle) Stop(_ string, container string, timeout int) error {
	operation := fmt.Sprintf("stop %s timeout=%d", container, timeout)
	f.record(operation)
	if f.injects(f.failBefore, operation) {
		return errors.New("injected stop failure")
	}
	if !f.exists[container] {
		return fmt.Errorf("missing container %s", container)
	}
	f.running[container] = false
	if f.injects(f.failAfter, operation) {
		return errors.New("injected stop response failure")
	}
	return nil
}

func (f *fakeContainerLifecycle) Remove(_ string, container string, _ bool) error {
	operation := "remove " + container
	f.record(operation)
	if f.injects(f.failBefore, operation) {
		return errors.New("injected remove failure")
	}
	if !f.exists[container] {
		return fmt.Errorf("missing container %s", container)
	}
	delete(f.exists, container)
	delete(f.running, container)
	if f.injects(f.failAfter, operation) {
		return errors.New("injected remove response failure")
	}
	return nil
}

func (f *fakeContainerLifecycle) Restart(_ string, container string, timeout int) error {
	f.record(fmt.Sprintf("restart %s timeout=%d", container, timeout))
	if !f.exists[container] {
		return fmt.Errorf("missing container %s", container)
	}
	f.running[container] = true
	f.captureMaxRunning()
	return nil
}

func (f *fakeContainerLifecycle) Exists(_ string, container string) (bool, error) {
	return f.exists[container], nil
}

func (f *fakeContainerLifecycle) IsRunning(_ string, container string) (bool, error) {
	if f.injects(f.failBefore, "inspect-running "+container) {
		return false, errors.New("injected running-state failure")
	}
	return f.running[container], nil
}

func (f *fakeContainerLifecycle) WaitRunning(_ string, container string, _ time.Duration) error {
	f.record("wait " + container)
	if f.failWait {
		return errors.New("injected startup failure")
	}
	if !f.running[container] {
		return fmt.Errorf("container %s is not running", container)
	}
	return nil
}

func (f *fakeContainerLifecycle) Rename(_ string, oldName, newName string) error {
	operation := "rename " + oldName + " " + newName
	f.record(operation)
	if f.injects(f.failBefore, operation) {
		return errors.New("injected rename failure")
	}
	if !f.exists[oldName] || f.exists[newName] {
		return fmt.Errorf("cannot rename %s to %s", oldName, newName)
	}
	f.exists[newName] = true
	f.running[newName] = f.running[oldName]
	delete(f.exists, oldName)
	delete(f.running, oldName)
	if f.injects(f.failAfter, operation) {
		return errors.New("injected rename response failure")
	}
	return nil
}

func (f *fakeContainerLifecycle) List(_ string, _ bool, _ map[string]string) ([]podman.Container, error) {
	names := make([]string, 0, len(f.exists))
	for name := range f.exists {
		names = append(names, name)
	}
	sort.Strings(names)
	containers := make([]podman.Container, 0, len(names))
	for _, name := range names {
		containers = append(containers, podman.Container{
			Name: name,
			Labels: map[string]string{
				"azud.service": "shop",
				"azud.role":    "worker",
			},
		})
	}
	return containers, nil
}

func (f *fakeContainerLifecycle) HostPort(_ string, _ string, _ int) (int, error) {
	return 0, nil
}

func operationIndex(operations []string, prefix string) int {
	for i, operation := range operations {
		if strings.HasPrefix(operation, prefix) {
			return i
		}
	}
	return -1
}

func stopFirstTestDeployer(fake *fakeContainerLifecycle) (*Deployer, *podman.ContainerConfig) {
	cfg := roleTestConfig()
	worker := cfg.Servers["worker"]
	worker.Hosts = []string{"host"}
	worker.Strategy = "stop_first"
	cfg.Servers["worker"] = worker
	containerCfg := NewAppContainerConfig(cfg, cfg.Image, "shop-worker-new", "worker", nil)
	return &Deployer{
		cfg:        cfg,
		containers: fake,
		log:        output.DefaultLogger,
	}, containerCfg
}

func TestStopFirstStandaloneRoleNeverOverlapsSingletons(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker")
	deployer, containerCfg := stopFirstTestDeployer(fake)

	if err := deployer.deployStopFirstStandaloneRole(
		"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
	); err != nil {
		t.Fatal(err)
	}

	if fake.maxRunning != 1 {
		t.Fatalf("singleton overlap detected; maximum running containers = %d; operations=%v", fake.maxRunning, fake.operations)
	}
	if !fake.exists["shop-worker"] || !fake.running["shop-worker"] {
		t.Fatalf("replacement did not receive the stable running name: exists=%v running=%v", fake.exists, fake.running)
	}
	if len(fake.exists) != 1 {
		t.Fatalf("temporary containers remain after successful replacement: %v", fake.exists)
	}
	stopIndex := operationIndex(fake.operations, "stop shop-worker-old-")
	runIndex := operationIndex(fake.operations, "run shop-worker-new")
	if stopIndex < 0 || runIndex < 0 || stopIndex >= runIndex {
		t.Fatalf("replacement started before previous singleton stopped: %v", fake.operations)
	}
}

func TestStopFirstStandaloneRoleRestoresPreviousAfterStartupFailure(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker")
	fake.failWait = true
	deployer, containerCfg := stopFirstTestDeployer(fake)

	err := deployer.deployStopFirstStandaloneRole(
		"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "previous singleton container restored") {
		t.Fatalf("expected restored startup failure, got %v", err)
	}
	if fake.maxRunning != 1 {
		t.Fatalf("singleton overlap detected during rollback; maximum running containers = %d; operations=%v", fake.maxRunning, fake.operations)
	}
	if !fake.exists["shop-worker"] || !fake.running["shop-worker"] || len(fake.exists) != 1 {
		t.Fatalf("previous singleton was not restored cleanly: exists=%v running=%v", fake.exists, fake.running)
	}
	stopNewIndex := operationIndex(fake.operations, "stop shop-worker-new")
	startOldIndex := operationIndex(fake.operations, "start shop-worker")
	if stopNewIndex < 0 || startOldIndex < 0 || stopNewIndex >= startOldIndex {
		t.Fatalf("previous singleton restarted before candidate stopped: %v", fake.operations)
	}
}

func TestStopFirstStandaloneRoleContinuesAfterConfirmedRunResponseFailure(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker")
	fake.failAfter["run shop-worker-new"] = true
	deployer, containerCfg := stopFirstTestDeployer(fake)

	if err := deployer.deployStopFirstStandaloneRole(
		"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
	); err != nil {
		t.Fatalf("confirmed run response failure should continue: %v", err)
	}
	if fake.maxRunning != 1 {
		t.Fatalf("ambiguous run response caused singleton overlap: max=%d operations=%v", fake.maxRunning, fake.operations)
	}
	if !fake.running["shop-worker"] || len(fake.exists) != 1 {
		t.Fatalf("replacement singleton was not activated after confirmed run: exists=%v running=%v", fake.exists, fake.running)
	}
}

func TestStopFirstStandaloneRoleFailsClosedWhenCandidateCannotBeConfirmedStopped(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker")
	fake.failAfter["run shop-worker-new"] = true
	fake.failBefore["inspect-running shop-worker-new"] = true
	deployer, containerCfg := stopFirstTestDeployer(fake)

	err := deployer.deployStopFirstStandaloneRole(
		"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "restore blocked until candidate state is known") {
		t.Fatalf("expected fail-closed restore error, got %v", err)
	}
	if fake.maxRunning != 1 {
		t.Fatalf("unconfirmed candidate stop caused singleton overlap: max=%d operations=%v", fake.maxRunning, fake.operations)
	}
	if fake.running["shop-worker"] {
		t.Fatalf("previous singleton restarted while candidate state was unsafe: running=%v", fake.running)
	}
	if !fake.running["shop-worker-new"] {
		t.Fatalf("test did not preserve the ambiguously running candidate: running=%v", fake.running)
	}
}

func TestStopFirstStandaloneRoleAcceptsConfirmedRemoteSideEffects(t *testing.T) {
	tests := []struct {
		name      string
		operation string
	}{
		{name: "create response", operation: "create shop-worker-new"},
		{name: "validated candidate removal response", operation: "remove shop-worker-new"},
		{name: "previous rename response", operation: "rename shop-worker shop-worker-old-"},
		{name: "previous stop response", operation: "stop shop-worker-old-"},
		{name: "replacement rename response", operation: "rename shop-worker-new shop-worker"},
		{name: "previous removal response", operation: "remove shop-worker-old-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeContainerLifecycle("shop-worker")
			fake.failAfter[tt.operation] = true
			deployer, containerCfg := stopFirstTestDeployer(fake)

			if err := deployer.deployStopFirstStandaloneRole(
				"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
			); err != nil {
				t.Fatalf("confirmed %s failure should not abort deployment: %v; operations=%v", tt.name, err, fake.operations)
			}
			if fake.maxRunning != 1 || !fake.running["shop-worker"] || len(fake.exists) != 1 {
				t.Fatalf("confirmed mutation broke singleton state: max=%d exists=%v running=%v", fake.maxRunning, fake.exists, fake.running)
			}
		})
	}
}

func TestStopFirstStandaloneRoleRestoresPreviousAfterMutationFailures(t *testing.T) {
	tests := []struct {
		name      string
		operation string
	}{
		{name: "create candidate", operation: "create shop-worker-new"},
		{name: "remove validated candidate", operation: "remove shop-worker-new"},
		{name: "preserve previous", operation: "rename shop-worker shop-worker-old-"},
		{name: "stop previous", operation: "stop shop-worker-old-"},
		{name: "run replacement", operation: "run shop-worker-new"},
		{name: "activate replacement", operation: "rename shop-worker-new shop-worker"},
		{name: "remove previous", operation: "remove shop-worker-old-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeContainerLifecycle("shop-worker")
			fake.failBefore[tt.operation] = true
			deployer, containerCfg := stopFirstTestDeployer(fake)

			err := deployer.deployStopFirstStandaloneRole(
				"host", "worker", "shop-worker", "shop-worker-new", containerCfg, true, false, nil,
			)
			if err == nil {
				t.Fatalf("expected injected %s failure", tt.name)
			}
			if fake.maxRunning != 1 || !fake.running["shop-worker"] {
				t.Fatalf("mutation failure did not preserve one active previous singleton: max=%d exists=%v running=%v operations=%v", fake.maxRunning, fake.exists, fake.running, fake.operations)
			}
		})
	}
}

func TestStopFirstStandaloneRoleFirstDeployment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := newFakeContainerLifecycle()
		deployer, containerCfg := stopFirstTestDeployer(fake)

		if err := deployer.deployStopFirstStandaloneRole(
			"host", "worker", "shop-worker", "shop-worker-new", containerCfg, false, false, nil,
		); err != nil {
			t.Fatal(err)
		}
		if !fake.running["shop-worker"] || len(fake.exists) != 1 {
			t.Fatalf("first singleton deployment did not activate stable container: exists=%v running=%v", fake.exists, fake.running)
		}
	})

	t.Run("startup failure", func(t *testing.T) {
		fake := newFakeContainerLifecycle()
		fake.failWait = true
		deployer, containerCfg := stopFirstTestDeployer(fake)

		err := deployer.deployStopFirstStandaloneRole(
			"host", "worker", "shop-worker", "shop-worker-new", containerCfg, false, false, nil,
		)
		if err == nil || len(fake.exists) != 0 {
			t.Fatalf("failed first deployment left a candidate: err=%v exists=%v", err, fake.exists)
		}
	})
}

func TestReconcileStopFirstRestoresRenamedPreviousContainer(t *testing.T) {
	backupName := "shop-worker-old-100"
	fake := newFakeContainerLifecycle(backupName)
	deployer, _ := stopFirstTestDeployer(fake)

	exists, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !fake.running["shop-worker"] || len(fake.exists) != 1 {
		t.Fatalf("renamed previous singleton was not recovered: exists=%v running=%v", fake.exists, fake.running)
	}
}

func TestReconcileStopFirstStopsInterruptedCandidateBeforeRestoringPrevious(t *testing.T) {
	backupName := "shop-worker-old-100"
	candidateName := "shop-worker-new-200"
	fake := newFakeContainerLifecycle(candidateName)
	fake.exists[backupName] = true
	fake.running[backupName] = false
	deployer, _ := stopFirstTestDeployer(fake)

	exists, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !fake.running["shop-worker"] || len(fake.exists) != 1 {
		t.Fatalf("interrupted replacement was not rolled back: exists=%v running=%v", fake.exists, fake.running)
	}
	stopCandidate := operationIndex(fake.operations, "stop "+candidateName)
	startPrevious := operationIndex(fake.operations, "start shop-worker")
	if stopCandidate < 0 || startPrevious < 0 || stopCandidate >= startPrevious {
		t.Fatalf("previous singleton started before interrupted candidate stopped: %v", fake.operations)
	}
}

func TestReconcileStopFirstCleansInterruptedFirstDeployment(t *testing.T) {
	candidateName := "shop-worker-new-200"
	fake := newFakeContainerLifecycle(candidateName)
	deployer, _ := stopFirstTestDeployer(fake)

	exists, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if exists || len(fake.exists) != 0 {
		t.Fatalf("interrupted first deployment was not cleaned: exists=%v", fake.exists)
	}
}

func TestReconcileStopFirstCleansTemporaryContainersBesideStable(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker", "shop-worker-new-200")
	fake.exists["shop-worker-old-100"] = true
	fake.running["shop-worker-old-100"] = false
	deployer, _ := stopFirstTestDeployer(fake)

	exists, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !fake.running["shop-worker"] || len(fake.exists) != 1 {
		t.Fatalf("temporary singleton containers were not cleaned: exists=%v running=%v", fake.exists, fake.running)
	}
}

func TestReconcileStopFirstStopsAllAmbiguousPreviousContainers(t *testing.T) {
	firstBackup := "shop-worker-old-100"
	secondBackup := "shop-worker-old-200"
	fake := newFakeContainerLifecycle(firstBackup, secondBackup)
	deployer, _ := stopFirstTestDeployer(fake)

	_, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err == nil || !strings.Contains(err.Error(), "multiple previous singleton containers") {
		t.Fatalf("expected manual reconciliation error, got %v", err)
	}
	if fake.running[firstBackup] || fake.running[secondBackup] {
		t.Fatalf("ambiguous previous containers were left running: %v", fake.running)
	}
	if len(fake.exists) != 2 {
		t.Fatalf("ambiguous previous containers should be preserved for manual recovery: %v", fake.exists)
	}
}

func TestReconcileStopFirstRejectsUnrecognizedManagedInstance(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker-0")
	deployer, _ := stopFirstTestDeployer(fake)

	_, err := deployer.reconcileStopFirstStandaloneRole("host", "worker", "shop-worker")
	if err == nil || !strings.Contains(err.Error(), "unrecognized managed container") {
		t.Fatalf("expected scaled singleton instance rejection, got %v", err)
	}
}

func TestRoleLifecycleCommandsUseRoleStopTimeout(t *testing.T) {
	fake := newFakeContainerLifecycle("shop-worker")
	deployer, _ := stopFirstTestDeployer(fake)
	worker := deployer.cfg.Servers["worker"]
	worker.Runtime.StopTimeout = 45 * time.Second
	deployer.cfg.Servers["worker"] = worker

	if err := deployer.StopRoles([]string{"host"}, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	if err := deployer.RestartRoles([]string{"host"}, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	if operationIndex(fake.operations, "stop shop-worker timeout=45") < 0 {
		t.Fatalf("role stop timeout was not used: %v", fake.operations)
	}
	if operationIndex(fake.operations, "restart shop-worker timeout=45") < 0 {
		t.Fatalf("role restart timeout was not used: %v", fake.operations)
	}
}
