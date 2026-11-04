//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A worker that runs only from the image that serves v1, so a later release fails on
// its second role, after its web role has switched. Roles deploy in name order.
const workerOnlyOnV1 = `grep -q v1 /www/index.html && exec sleep 3600`

// fastStops makes a service's containers stop in a second: these apps ignore SIGTERM
// as PID 1, and these tests replace them many times.
func fastStops(config string) string {
	return strings.Replace(config, "deploy:\n", "deploy:\n  stop_timeout: 1s\n", 1)
}

// withWorker adds a worker role running cmd to a service's config on disk.
func (s *service) withWorker(cmd string) {
	s.t.Helper()
	config := strings.Replace(s.renderConfig(), "      - 127.0.0.1\n", "      - 127.0.0.1\n  worker:\n    hosts:\n      - 127.0.0.1\n    cmd: "+strconv.Quote(cmd)+"\n", 1)
	if err := os.WriteFile(s.config, []byte(fastStops(config)), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *service) workerContainer() string { return s.name + "-worker" }

func (s *service) imageOf(container string) string {
	s.t.Helper()
	out, err := podman("container", "inspect", "--format", "{{.ImageName}}", container)
	if err != nil {
		s.t.Fatalf("inspect %s: %v\n%s", container, err, out)
	}
	return strings.TrimSpace(out)
}

// killAgentAt runs an agent check and kills it, with SIGKILL, when it reaches a podman
// command matching pattern, a shell case pattern. Nothing after that point runs: the
// command it reached stops with it.
func (s *service) killAgentAt(pattern string) {
	s.t.Helper()
	s.killAt(pattern, "agent", "run", "--once")
}

// killAt runs azud with args and kills it as killAgentAt does.
func (s *service) killAt(pattern string, args ...string) {
	s.t.Helper()
	realPodman, err := exec.LookPath("podman")
	if err != nil {
		s.t.Fatal(err)
	}
	bin := filepath.Join(s.dir, "killbin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		s.t.Fatal(err)
	}
	reached := filepath.Join(s.dir, "reached")
	_ = os.Remove(reached)
	shim := "#!/bin/sh\ncase \"$*\" in\n  " + pattern + ") echo $$ > " + reached + "; exec sleep 300 ;;\nesac\nexec " + realPodman + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(shim), 0o755); err != nil {
		s.t.Fatal(err)
	}
	logPath := filepath.Join(s.dir, "killed-agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()

	command := exec.Command(h.binary, append(args, "-c", s.config)...)
	command.Dir = s.dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile, "PATH="+bin+":"+os.Getenv("PATH"))
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		s.t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	deadline := time.After(3 * time.Minute)
	for {
		if data, err := os.ReadFile(reached); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			if err := command.Process.Kill(); err != nil {
				s.t.Fatal(err)
			}
			<-exited
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			return
		}
		select {
		case err := <-exited:
			out, _ := os.ReadFile(logPath)
			s.t.Fatalf("the agent exited (%v) before reaching %q:\n%s", err, pattern, out)
		case <-deadline:
			out, _ := os.ReadFile(logPath)
			s.t.Fatalf("the agent did not reach %q:\n%s", pattern, out)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// editState changes the agent's state file on disk.
func (s *service) editState(edit func(map[string]any)) {
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
	edit(st)
	if data, err = json.Marshal(st); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// stopRegistry takes the registry away until the test ends.
func stopRegistry(t *testing.T) {
	t.Helper()
	if out, err := podman("stop", registryContainer); err != nil {
		t.Fatalf("stop registry: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := podman("start", registryContainer); err != nil {
			t.Fatalf("start registry: %v\n%s", err, out)
		}
		if err := h.waitForRegistry(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestImageReleaseFailingOnItsSecondRoleRollsBackAllRoles(t *testing.T) {
	s := newService(t, "secondrole")
	s.withWorker(workerOnlyOnV1)
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	watch := s.watchRequests("v1", "v2")
	second := s.release("v2")
	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("agent reported success for a release whose worker cannot run:\n%s", out)
	}
	if !strings.Contains(out, "web role deployed successfully") {
		t.Fatalf("the web role never switched, so this proves little:\n%s", out)
	}
	if !strings.Contains(out, "Rolling back to release") {
		t.Fatalf("no rollback in the output:\n%s", out)
	}
	watch.stop()
	watch.requireNoFailures()

	s.waitServes("v1")
	for _, container := range []string{s.roleContainer(), s.workerContainer()} {
		if got, want := s.imageOf(container), s.repo+"@"+first; got != want {
			t.Errorf("%s runs %q, want the previous release %q", container, got, want)
		}
	}
	st := s.agentState()
	if st.DeployedDigest != first || st.FailedDigest != second || st.Journal != nil {
		t.Errorf("state = %+v, want deployed %s, failed %s and no open journal", st, first, second)
	}
	if !strings.HasPrefix(st.Current, strings.TrimPrefix(first, "sha256:")+"-") {
		t.Errorf("current = %q, want a record of %s", st.Current, first)
	}

	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("next check = %v, want the failed release skipped:\n%s", err, out)
	}
	s.waitServes("v1")
}

func TestFailedFirstImageReleaseStopsWhatItStarted(t *testing.T) {
	s := newService(t, "firstimage")
	s.withWorker("false")
	digest := s.release("v1")

	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("agent reported success for a release whose worker cannot run:\n%s", out)
	}
	if !strings.Contains(out, "Stopping roles the failed first release started") {
		t.Fatalf("the failure did not reach the cleanup of a started role, so this proves little:\n%s", out)
	}
	if got, err := podman("inspect", "--format", "{{.State.Running}}", s.roleContainer()); err == nil && strings.TrimSpace(got) == "true" {
		t.Errorf("the web role of a failed first release is still running:\n%s", out)
	}
	st := s.agentState()
	if st.DeployedDigest != "" || st.FailedDigest != digest || st.Journal != nil {
		t.Errorf("state = %+v, want nothing deployed, failed %s and no open journal", st, digest)
	}
}

// An agent killed after the web role switched and before the worker did: the server
// runs parts of two releases until the next check finishes the release.
func TestKilledAgentResumesOnItsNextCheck(t *testing.T) {
	s := newService(t, "killedagent")
	s.withWorker("sleep 3600")
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	second := s.release("v2")
	s.killAgentAt("*" + s.workerContainer() + "-new*")

	st := s.agentState()
	if st.Journal == nil || st.Journal.Phase != "forward" || st.Journal.Digest != second || st.DeployedDigest != first {
		t.Fatalf("state after the kill = %+v, journal %+v", st, st.Journal)
	}
	if got, want := s.imageOf(s.roleContainer()), s.repo+"@"+second; got != want {
		t.Fatalf("web runs %q, want %q: the kill must leave a mixed server", got, want)
	}
	if got, want := s.imageOf(s.workerContainer()), s.repo+"@"+first; got != want {
		t.Fatalf("worker runs %q, want %q: the kill must leave a mixed server", got, want)
	}

	// Verification would now fail: the record was verified before it was stored.
	if err := os.Remove(filepath.Join(s.signed, second)); err != nil {
		t.Fatal(err)
	}
	out := s.deploys()
	if !strings.Contains(out, "Resuming the interrupted deploy") {
		t.Errorf("the check did not resume:\n%s", out)
	}
	s.waitServes("v2")
	for _, container := range []string{s.roleContainer(), s.workerContainer()} {
		if got, want := s.imageOf(container), s.repo+"@"+second; got != want {
			t.Errorf("%s runs %q, want %q", container, got, want)
		}
	}
	st = s.agentState()
	if st.DeployedDigest != second || st.FailedDigest != "" || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v, want %s deployed and the journal closed", st, st.Journal, second)
	}
	if !strings.HasPrefix(st.Current, strings.TrimPrefix(second, "sha256:")+"-") || len(st.Applied) != 2 {
		t.Errorf("current = %q, applied = %v", st.Current, st.Applied)
	}

	before := s.inspect("{{.Id}}")
	s.deploys()
	if after := s.inspect("{{.Id}}"); after != before {
		t.Errorf("a check after the resume replaced the container: %s to %s", before, after)
	}
}

// An agent killed in the middle of one role's swap: the new container is on the proxy
// and the old one is on its way out.
func TestAgentKilledDuringASwapResumes(t *testing.T) {
	s := newService(t, "killedswap")
	if err := os.WriteFile(s.config, []byte(fastStops(s.renderConfig())), 0o644); err != nil {
		t.Fatal(err)
	}
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	second := s.release("v2")
	s.killAgentAt("rename*")

	st := s.agentState()
	if st.Journal == nil || st.Journal.Digest != second || st.DeployedDigest != first {
		t.Fatalf("state after the kill = %+v, journal %+v", st, st.Journal)
	}

	s.deploys()
	s.waitServes("v2")
	if got, want := s.inspect("{{.ImageName}}"), s.repo+"@"+second; got != want {
		t.Errorf("container runs %q, want %q", got, want)
	}
	st = s.agentState()
	if st.DeployedDigest != second || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v", st, st.Journal)
	}
	out, err := podman("ps", "-a", "--filter", "label=azud.service="+s.name, "--format", "{{.Names}}")
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Fields(out); len(names) != 1 || names[0] != s.roleContainer() {
		t.Errorf("containers after the resume = %v, want only %s", names, s.roleContainer())
	}
	// A dead upstream left on the route would show as failed requests here.
	watch := s.watchRequests("v2")
	time.Sleep(5 * time.Second)
	watch.stop()
	watch.requireNoFailures()
}

func TestResumeNeedsNoRegistry(t *testing.T) {
	s := newService(t, "resumeoffline")
	s.withWorker("sleep 3600")
	s.release("v1")
	s.deploys()
	s.waitServes("v1")

	second := s.release("v2")
	s.killAgentAt("*" + s.workerContainer() + "-new*")

	stopRegistry(t)
	out := s.deploys()
	if !strings.Contains(out, "Resuming the interrupted deploy") {
		t.Errorf("the check did not resume:\n%s", out)
	}
	s.waitServes("v2")
	st := s.agentState()
	if st.DeployedDigest != second || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v", st, st.Journal)
	}
}

func TestUndoNeedsNoRegistry(t *testing.T) {
	s := newService(t, "undooffline")
	s.withWorker("sleep 3600")
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	second := s.release("v2")
	s.killAgentAt("*" + s.workerContainer() + "-new*")
	s.editState(func(st map[string]any) { st["failed_digest"] = second })

	stopRegistry(t)
	if out, err := s.agentOnce(); err != nil {
		t.Fatalf("undo with the registry down: %v\n%s", err, out)
	}
	s.waitServes("v1")
	for _, container := range []string{s.roleContainer(), s.workerContainer()} {
		if got, want := s.imageOf(container), s.repo+"@"+first; got != want {
			t.Errorf("%s runs %q, want %q", container, got, want)
		}
	}
	st := s.agentState()
	if st.DeployedDigest != first || st.FailedDigest != second || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v", st, st.Journal)
	}
}

func TestDeployCommandFailureIsRecordedWithoutAnUndo(t *testing.T) {
	s := newService(t, "commandfail")
	ran := filepath.Join(s.dir, "ran")
	config := strings.Replace(s.renderConfig(), "agent:\n", fmt.Sprintf(`agent:
  deploy_command: |-
    echo "$$AZUD_AGENT_DIGEST" >> %s
    test "$$AZUD_AGENT_DIGEST" = "$$(cat %s)"
`, ran, filepath.Join(s.dir, "allowed")), 1)
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	first := s.publish("v1")
	s.sign(first)
	if err := os.WriteFile(filepath.Join(s.dir, "allowed"), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	s.promote(first)
	s.deploys()

	second := s.release("v2")
	out, err := s.agentOnce()
	if err == nil {
		t.Fatalf("the agent reported success for a failed deploy_command:\n%s", out)
	}
	data, err := os.ReadFile(ran)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("deploy_command ran for %v, want once each and no run to undo it", got)
	}
	st := s.agentState()
	if st.DeployedDigest != first || st.FailedDigest != second || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v, want deployed %s, failed %s, journal closed", st, st.Journal, first, second)
	}
	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("next check = %v, want the failed release skipped:\n%s", err, out)
	}
}
