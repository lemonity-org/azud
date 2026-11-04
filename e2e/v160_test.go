//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// v160Check runs one check with a real v1.6.0 binary, on the same state and containers.
func v160Check(s *service, binary string) (string, error) {
	command := exec.Command(binary, "agent", "run", "--once", "-c", s.config)
	command.Dir = s.dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile)
	out, err := command.CombinedOutput()
	return string(out), err
}

// A real v1.6.0 binary and this one taking turns on the same server, which is the upgrade
// and the way back. AZUD_E2E_V160 names a binary built from the v1.6.0 tag.
func TestV160BinarySwap(t *testing.T) {
	old := os.Getenv("AZUD_E2E_V160")
	if old == "" {
		t.Skip("AZUD_E2E_V160 is not set")
	}
	s := newBundleService(t, "v160swap")
	// Only later versions create the network themselves.
	if out, err := podman("network", "create", "--ignore", "azud"); err != nil {
		t.Fatalf("create the azud network: %v\n%s", err, out)
	}

	first := s.publishBundle(s.publish("v1"), "one", nil)
	s.promote(first)
	if out, err := v160Check(s.service, old); err != nil {
		t.Fatalf("v1.6.0 first deploy: %v\n%s", err, out)
	}
	s.waitServes("v1")
	stored := filepath.Join(s.state, "agent", s.name, "releases", strings.TrimPrefix(first, "sha256:"))
	if _, err := os.Stat(filepath.Join(stored, "record.json")); !os.IsNotExist(err) {
		t.Fatalf("v1.6.0 wrote a record? %v", err)
	}

	// Swapped to this binary: nothing to do, and current is derived.
	before := s.inspect("{{.Id}}")
	s.deploys()
	if after := s.inspect("{{.Id}}"); after != before {
		t.Errorf("the first check after the upgrade replaced the container")
	}
	st := s.agentState()
	if st.DeployedDigest != first || st.Current != strings.TrimPrefix(first, "sha256:") {
		t.Errorf("state after upgrade = %+v", st)
	}

	// A rollback to the release v1.6.0 stored, which has no record file, with the
	// registry down. It is not verified again, and the agent holds what it left.
	second := s.publishBundle(s.publish("v1b"), "oneb", nil)
	s.promote(second)
	s.deploys()
	s.waitServes("v1b")
	withRegistryStopped(t, func() {
		s.mustAzud("rollback")
		s.waitServes("v1")
	})
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets after the rollback = %q", got)
	}
	s.requireDeployed(first, second)

	// v1.6.0 reads the hold as a failed release.
	if out, err := v160Check(s.service, old); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("v1.6.0 after a rollback = %v\n%s", err, out)
	}

	// A failure rolls back to the release v1.6.0 stored, which has no record file.
	bad := s.publishBundle(s.publishBroken("v2"), "two", nil)
	s.promote(bad)
	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("agent reported success for a broken release:\n%s", out)
	}
	s.waitServes("v1")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	st = s.agentState()
	if st.DeployedDigest != first || st.FailedDigest != bad || st.Journal != nil {
		t.Errorf("state after the failure = %+v, journal %+v", st, st.Journal)
	}

	// Back on v1.6.0: it reads what this binary wrote.
	out, err = v160Check(s.service, old)
	if err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("v1.6.0 after the downgrade = %v\n%s", err, out)
	}
	fixed := s.publishBundle(s.publish("v3"), "three", nil)
	s.promote(fixed)
	if out, err := v160Check(s.service, old); err != nil {
		t.Fatalf("v1.6.0 deploy after the downgrade: %v\n%s", err, out)
	}
	s.waitServes("v3")
	if got := s.agentState().DeployedDigest; got != fixed {
		t.Errorf("deployed = %q, want %q", got, fixed)
	}

	// Again on this binary.
	s.deploys()
	st = s.agentState()
	if st.DeployedDigest != fixed || st.Current != strings.TrimPrefix(fixed, "sha256:") || st.FailedDigest != "" {
		t.Errorf("state after the second upgrade = %+v", st)
	}

	// This binary stores a release, then v1.6.0 returns to it after a failure.
	fourth := s.publishBundle(s.publish("v4"), "four", nil)
	s.promote(fourth)
	s.deploys()
	s.waitServes("v4")
	bad2 := s.publishBundle(s.publishBroken("v5"), "five", nil)
	s.promote(bad2)
	if out, err := v160Check(s.service, old); err == nil {
		t.Fatalf("v1.6.0 reported success for a broken release:\n%s", out)
	}
	s.waitServes("v4")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=four") {
		t.Errorf("runtime secrets after v1.6.0's rollback to a release this binary stored = %q", got)
	}
}
