//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bundleService is a service whose agent follows release bundles.
type bundleService struct {
	*service
	serverConfig string
	secretsFile  string
	recipient    string
	published    int
}

func newBundleService(t *testing.T, name string) *bundleService {
	t.Helper()
	s := &bundleService{service: newService(t, name)}
	s.secretsFile = filepath.Join(s.dir, "runtime", "secrets")
	if err := os.MkdirAll(filepath.Dir(s.secretsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	s.serverConfig = strings.Replace(s.renderConfig(), "agent:\n", "secrets_remote_path: "+s.secretsFile+"\n\nagent:\n  source: bundle\n  identity: "+filepath.Join(s.dir, "agent.age")+"\n", 1)
	s.writeServerConfig()

	command := exec.Command(h.binary, "agent", "init", "-c", s.config)
	command.Dir = s.dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("agent init: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "age1") {
			s.recipient = strings.TrimSpace(line)
		}
	}
	if s.recipient == "" {
		t.Fatalf("agent init printed no recipient:\n%s", out)
	}
	return s
}

func (s *bundleService) writeServerConfig() {
	s.t.Helper()
	if err := os.WriteFile(s.config, []byte(s.serverConfig), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// publishBundle publishes and signs a bundle for image, with one secret. edit changes
// the repository's config, which is what the bundle carries. It does not move the tag.
func (s *bundleService) publishBundle(image, secret string, edit func(string) string) string {
	s.t.Helper()
	s.published++
	dir := filepath.Join(s.dir, "repo", strings.Repeat("n", s.published))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	repoConfig := s.serverConfig + "\nenv:\n  secret: [APP_SECRET]\n\nsecrets_path: secrets\n\nrelease:\n  recipients: [" + s.recipient + "]\n"
	if edit != nil {
		repoConfig = edit(repoConfig)
	}
	config := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(config, []byte(repoConfig), 0o644); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte("APP_SECRET="+secret+"\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
	digestFile := filepath.Join(dir, "digest")
	command := exec.Command(h.binary, "release", "publish", "-c", config, "--image", image, "--digest-file", digestFile)
	command.Dir = dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile)
	if out, err := command.CombinedOutput(); err != nil {
		s.t.Fatalf("release publish: %v\n%s", err, out)
	}
	digest, err := os.ReadFile(digestFile)
	if err != nil {
		s.t.Fatal(err)
	}
	bundle := strings.TrimSpace(string(digest))
	s.sign(bundle)
	return bundle
}

func (s *bundleService) pendingFile() string {
	return filepath.Join(s.state, "agent", s.name, "releases", "pending")
}

func (s *bundleService) running(container string) bool {
	out, err := podman("inspect", "--format", "{{.State.Running}}", container)
	return err == nil && strings.TrimSpace(out) == "true"
}

func (s *bundleService) runtimeSecrets() string {
	s.t.Helper()
	data, err := os.ReadFile(s.secretsFile)
	if err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
	}
	return string(data)
}

// seedFailedRollback leaves on disk what a failed rollback leaves: the failed release
// is running, the record names the previous one, and the deploy is still pending.
func (s *bundleService) seedFailedRollback(previous, failed string) {
	s.t.Helper()
	path := filepath.Join(s.state, "agent", s.name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		s.t.Fatal(err)
	}
	st["deployed_digest"], st["failed_digest"] = previous, failed
	if data, err = json.Marshal(st); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(s.pendingFile(), []byte(failed+"\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func TestBundleDeploysItsImageAndSecrets(t *testing.T) {
	s := newBundleService(t, "bundlefirst")
	digest := s.publishBundle(s.publish("v1"), "one", nil)
	s.promote(digest)

	s.deploys()

	s.waitServes("v1")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	if got := s.agentState().DeployedDigest; got != digest {
		t.Errorf("deployed digest = %q, want %q", got, digest)
	}
}

func TestFailedFirstBundleStopsWhatItStarted(t *testing.T) {
	s := newBundleService(t, "bundlefail")
	withBrokenWorker := func(config string) string {
		return strings.Replace(config, "      - 127.0.0.1\n", "      - 127.0.0.1\n  worker:\n    hosts:\n      - 127.0.0.1\n    cmd: \"false\"\n", 1)
	}
	digest := s.publishBundle(s.publish("v1"), "one", withBrokenWorker)
	s.promote(digest)

	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("agent reported success for a release whose worker cannot run:\n%s", out)
	}
	if !strings.Contains(out, "Stopping roles the failed first release started") {
		t.Fatalf("the failure did not reach the cleanup of a started role, so this proves little:\n%s", out)
	}
	if s.running(s.roleContainer()) {
		t.Errorf("the web role of a failed first release is still running:\n%s", out)
	}
	if got := s.runtimeSecrets(); got != "" {
		t.Errorf("runtime secrets = %q; the file did not exist before this release", got)
	}
	if _, err := os.Stat(s.pendingFile()); !os.IsNotExist(err) {
		t.Errorf("deploy still marked interrupted after its cleanup: %v", err)
	}

	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("next check = %v, want the failed release skipped:\n%s", err, out)
	}
}

// v1.6.0 stayed in this state until the tag moved.
func TestFailedRollbackRecoversWithoutTheTagMoving(t *testing.T) {
	s := newBundleService(t, "bundlerecover")
	first := s.publishBundle(s.publish("v1"), "one", nil)
	s.promote(first)
	s.deploys()
	s.waitServes("v1")

	second := s.publishBundle(s.publish("v2"), "two", nil)
	s.promote(second)
	s.deploys()
	s.waitServes("v2")

	s.seedFailedRollback(first, second)
	if out, err := s.agentOnce(); err != nil {
		t.Fatalf("recovery: %v\n%s", err, out)
	}

	s.waitServes("v1")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q, want release 1's", got)
	}
	if _, err := os.Stat(s.pendingFile()); !os.IsNotExist(err) {
		t.Errorf("deploy still marked interrupted after recovery: %v", err)
	}
	if st := s.agentState(); st.DeployedDigest != first || st.FailedDigest != second {
		t.Errorf("state = %+v, want deployed %s and failed %s", st, first, second)
	}
	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("next check = %v, want the failed release skipped:\n%s", err, out)
	}
	s.waitServes("v1")
}

// The server leaves ssh.command_timeout to its default of 35s. Stopping the old
// container takes the release's whole 40s, since the app ignores SIGTERM as PID 1.
func TestBundleWithALongerStopTimeoutIsNotCutShort(t *testing.T) {
	s := newBundleService(t, "bundlestop")
	s.serverConfig = strings.Replace(s.serverConfig, "  command_timeout: 5m\n", "", 1)
	s.writeServerConfig()
	withStopTimeout := func(config string) string {
		return strings.Replace(config, "deploy:\n", "deploy:\n  stop_timeout: 40s\n", 1)
	}

	s.promote(s.publishBundle(s.publish("v1"), "one", withStopTimeout))
	s.deploys()
	s.waitServes("v1")

	second := s.publishBundle(s.publish("v2"), "two", withStopTimeout)
	s.promote(second)
	s.deploys()
	s.waitServes("v2")
	if got := s.agentState().DeployedDigest; got != second {
		t.Errorf("deployed digest = %q, want %q", got, second)
	}
}

func TestDeployCommandGetsTheLockItRunsUnder(t *testing.T) {
	s := newService(t, "lockenv")
	result := filepath.Join(s.dir, "lock-result")
	config := strings.Replace(s.renderConfig(), "agent:\n", `agent:
  deploy_command: |-
    test -n "$$AZUD_AGENT_IMAGE"
    printf '%s\n' "$$AZUD_DEPLOY_LOCK" > `+result+`
    flock -n "$$AZUD_DEPLOY_LOCK" true
    echo "$$?" >> `+result+`
`, 1)
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	s.release("v1")
	s.deploys()

	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 2 {
		t.Fatalf("lock result = %q", data)
	}
	if !filepath.IsAbs(lines[0]) || strings.Contains(lines[0], "$") {
		t.Errorf("AZUD_DEPLOY_LOCK = %q, want an absolute path", lines[0])
	}
	if lines[1] != "1" {
		t.Errorf("flock -n exited %s, want 1: AZUD_DEPLOY_LOCK must name the lock the agent holds", lines[1])
	}
}
