//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstDeploy(t *testing.T) {
	s := newService(t, "first")
	digest := s.release("v1")

	s.deploys()

	s.waitServes("v1")
	if got := s.agentState().DeployedDigest; got != digest {
		t.Errorf("deployed digest = %q, want %q", got, digest)
	}
	if got, want := s.inspect("{{.ImageName}}"), s.repo+"@"+digest; got != want {
		t.Errorf("container runs %q, want it pinned to %q", got, want)
	}

	before := s.inspect("{{.Id}}")
	s.deploys()
	if after := s.inspect("{{.Id}}"); after != before {
		t.Errorf("a second check with the tag unchanged replaced the container: %s to %s", before, after)
	}
}

func TestUnverifiedImageIsNotDeployed(t *testing.T) {
	s := newService(t, "unverified")
	good := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	unsigned := s.publish("v2")
	s.promote(unsigned)

	if out, err := s.agentOnce(); err == nil {
		t.Fatalf("agent accepted an image that failed verify_command:\n%s", out)
	}
	s.waitServes("v1")
	st := s.agentState()
	if st.DeployedDigest != good {
		t.Errorf("deployed digest = %q, want %q", st.DeployedDigest, good)
	}
	if st.FailedDigest != unsigned {
		t.Errorf("failed digest = %q, want %q", st.FailedDigest, unsigned)
	}
}

func TestNewReleaseIsZeroDowntime(t *testing.T) {
	s := newService(t, "rolling")
	s.release("v1")
	s.deploys()
	s.waitServes("v1")

	watch := s.watchRequests("v1", "v2")

	s.release("v2")
	s.deploys()
	s.waitServes("v2")
	watch.stop()
	watch.requireNoFailures()
}

func TestBadReleaseKeepsThePreviousOneServing(t *testing.T) {
	s := newService(t, "badrelease")
	good := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	// The undo replaces the serving container with one of the same release, so what
	// must hold is that the bad release never takes a request and none fails.
	watch := s.watchRequests("v1")
	bad := s.publishBroken("v2")
	s.sign(bad)
	s.promote(bad)
	if out, err := s.agentOnce(); err == nil {
		t.Fatalf("agent reported success for a release that never became ready:\n%s", out)
	}
	watch.stop()
	watch.requireNoFailures()

	s.waitServes("v1")
	if got, want := s.inspect("{{.ImageName}}"), s.repo+"@"+good; got != want {
		t.Errorf("after a release that failed readiness, the container runs %q, want %q", got, want)
	}
	st := s.agentState()
	if st.DeployedDigest != good || st.FailedDigest != bad {
		t.Errorf("state = %+v, want deployed %s and failed %s", st, good, bad)
	}

	// A one-shot check reports a known failed release as an error, and does not retry it.
	running := s.inspect("{{.Id}}")
	out, err := s.agentOnce()
	if err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("a check on a known failed release should refuse it, got %v\n%s", err, out)
	}
	if after := s.inspect("{{.Id}}"); after != running {
		t.Errorf("a refused release replaced the serving container: %s to %s", running, after)
	}
	s.waitServes("v1")

	fixed := s.release("v3")
	s.deploys()
	s.waitServes("v3")
	st = s.agentState()
	if st.DeployedDigest != fixed || st.FailedDigest != "" {
		t.Errorf("after a fix, state = %+v, want deployed %s and nothing failed", st, fixed)
	}
	if !strings.Contains(s.inspect("{{.ImageName}}"), fixed) {
		t.Errorf("container runs %q, want %s", s.inspect("{{.ImageName}}"), fixed)
	}
}

// A podman on the agent's PATH makes pulls slower than ssh.command_timeout, with
// real pulls behind it. The first check keeps the limit below the pull, so it must
// fail and say why. The second has the default pull_timeout and must finish.
func TestSlowPullIsNotCutShort(t *testing.T) {
	s := newService(t, "slowpull")
	realPodman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(s.dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nif [ \"$1\" = pull ]; then sleep 14; fi\nexec " + realPodman + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	s.agentEnv = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}

	// Stops take 1s, so a 12s limit is enough for everything but the pull.
	config := strings.Replace(s.renderConfig(), "  command_timeout: 5m\n", "  command_timeout: 12s\n", 1)
	config = strings.Replace(config, "deploy:\n", "deploy:\n  stop_timeout: 1s\n", 1)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(s.config, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(strings.Replace(config, "deploy:\n", "deploy:\n  pull_timeout: 13s\n", 1))

	digest := s.release("v1")
	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("the check passed with a pull limit below the pull's length:\n%s", out)
	}
	if !strings.Contains(out, "timed out after 13s") {
		t.Errorf("the failed pull does not say why:\n%s", out)
	}

	write(config)
	if out, err := s.agentOnce("--retry-failed"); err != nil {
		t.Fatalf("a pull within the default pull_timeout failed: %v\n%s", err, out)
	}
	s.waitServes("v1")
	if got := s.agentState().DeployedDigest; got != digest {
		t.Errorf("deployed digest = %q, want %q", got, digest)
	}
}
