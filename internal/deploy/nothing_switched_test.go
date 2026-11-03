package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/localtest"
	"github.com/lemonity-org/azud/internal/output"
)

// twoRoleDeployer deploys the roles alpha and beta, whose commands answer as told.
// A nil answer means success.
func twoRoleDeployer(t *testing.T, answer func(cmd string) (string, int)) (*Deployer, *localtest.Fake) {
	t.Helper()
	t.Setenv("AZUD_STATE_DIR", t.TempDir())
	cfg := &config.Config{
		Service: "shop",
		Image:   "example/shop@sha256:abc",
		Servers: map[string]config.RoleConfig{
			"alpha": {Hosts: []string{"host"}, Cmd: "a"},
			"beta":  {Hosts: []string{"host"}, Cmd: "b"},
		},
		HooksPath: t.TempDir(),
		Deploy:    config.DeployConfig{ReadinessDelay: time.Millisecond},
	}
	fake := &localtest.Fake{
		Handle: func(cmd string, _ io.Reader) (string, string, int) {
			if out, code := answer(cmd); out != "" || code != 0 {
				return out, "failed: " + out, code
			}
			if strings.Contains(cmd, "RestartCount") {
				return "true 0 0", "", 0
			}
			return "", "", 0
		},
		UserName: "deploy",
	}
	return NewDeployer(cfg, fake, output.NewLogger(io.Discard, io.Discard, false)), fake
}

func deployWithStop(d *Deployer) error {
	return d.Deploy(context.Background(), &DeployOptions{Hosts: []string{"host"}, LockHeld: true, StopOnFailure: true})
}

func ranContainer(fake *localtest.Fake, role string) bool {
	for _, cmd := range fake.Commands() {
		if strings.Contains(cmd, " run ") && strings.Contains(cmd, "shop-"+role) {
			return true
		}
	}
	return false
}

func TestFailedPullSwitchesNothing(t *testing.T) {
	d, fake := twoRoleDeployer(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "pull") {
			return "manifest unknown", 1
		}
		return "", 0
	})
	err := deployWithStop(d)
	if !errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("Deploy = %v, want ErrNothingSwitched", err)
	}
	if !strings.Contains(err.Error(), "failed to pull image") || strings.Contains(err.Error(), ErrNothingSwitched.Error()) {
		t.Errorf("error reads %q; it should read as its cause", err)
	}
	if ranContainer(fake, "alpha") {
		t.Error("a role started after the pull failed")
	}
}

func TestFirstRoleFailingItsStartupCheckSwitchesNothing(t *testing.T) {
	d, fake := twoRoleDeployer(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "RestartCount") && strings.Contains(cmd, "shop-alpha") {
			return "false 0 1", 0
		}
		return "", 0
	})
	err := deployWithStop(d)
	if !errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("Deploy = %v, want ErrNothingSwitched", err)
	}
	if ranContainer(fake, "beta") {
		t.Error("a later role switched to a release that had already failed")
	}
}

func TestSecondRoleFailingLeavesTheFirstSwitched(t *testing.T) {
	d, _ := twoRoleDeployer(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "RestartCount") && strings.Contains(cmd, "shop-beta") {
			return "false 0 1", 0
		}
		return "", 0
	})
	err := deployWithStop(d)
	if err == nil || errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("Deploy = %v, want a failure with a role switched", err)
	}
}

func TestStopFirstRoleFailingCountsAsSwitched(t *testing.T) {
	d, _ := twoRoleDeployer(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "RestartCount") {
			return "false 0 1", 0
		}
		return "", 0
	})
	role := d.cfg.Servers["alpha"]
	role.Strategy = config.StrategyStopFirst
	d.cfg.Servers["alpha"] = role

	err := deployWithStop(d)
	if err == nil || errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("Deploy = %v; a stop-first role's old container is already stopped", err)
	}
}

func TestPreAppBootHookFailingSwitchesNothing(t *testing.T) {
	d, _ := twoRoleDeployer(t, func(string) (string, int) { return "", 0 })
	if err := os.WriteFile(filepath.Join(d.cfg.HooksPath, "pre-app-boot"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := deployWithStop(d); !errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("Deploy = %v, want ErrNothingSwitched", err)
	}
}

func TestFailureAfterTheProxyChangedCountsAsSwitched(t *testing.T) {
	cfg := roleTestConfig()
	cfg.Proxy.Host = "new.example.com"
	proxyManager := &fakeDeploymentProxy{ensureHostsErr: errors.New("admin API refused")}
	deployer := &Deployer{
		cfg:        cfg,
		containers: newFakeContainerLifecycle("shop"),
		proxy:      proxyManager,
		hooks:      NewHookRunner(t.TempDir(), 0, output.DefaultLogger),
		log:        output.DefaultLogger,
	}
	err := deployer.deployToTargetLocked(context.Background(), deploymentTarget{Host: "host", Role: "web"}, cfg.Image, "latest", &DeployOptions{SkipHealthCheck: true})
	if err == nil || errors.Is(err, ErrNothingSwitched) {
		t.Fatalf("deploy = %v; the route's hosts may already have changed, so this needs the undo", err)
	}
}
