//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// withRegistryStopped runs fn with the registry down, and brings it back whatever fn does.
func withRegistryStopped(t *testing.T, fn func()) {
	t.Helper()
	if out, err := podman("stop", registryContainer); err != nil {
		t.Fatalf("stop registry: %v\n%s", err, out)
	}
	defer func() {
		if out, err := podman("start", registryContainer); err != nil {
			t.Errorf("start registry: %v\n%s", err, out)
			return
		}
		if err := h.waitForRegistry(); err != nil {
			t.Error(err)
		}
	}()
	fn()
}

func (s *service) writeConfig(config string) {
	s.t.Helper()
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// fastConfig makes the service's containers stop in a second.
func (s *service) fastConfig() {
	s.t.Helper()
	s.writeConfig(fastStops(s.renderConfig()))
}

func (s *service) requireDeployed(deployed, failed string) {
	s.t.Helper()
	st := s.agentState()
	if st.DeployedDigest != deployed || st.FailedDigest != failed || st.Journal != nil {
		s.t.Fatalf("state = %+v, journal %+v; want deployed %s, failed %q, no journal", st, st.Journal, deployed, failed)
	}
}

func (s *service) requireImage(container, digest string) {
	s.t.Helper()
	if got, want := s.imageOf(container), s.repo+"@"+digest; got != want {
		s.t.Errorf("%s runs %q, want %q", container, got, want)
	}
}

func TestRollbackHoldsFollowsAFixAndNeedsNoRegistry(t *testing.T) {
	s := newService(t, "rollbackhold")
	s.fastConfig()
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")
	second := s.release("v2")
	s.deploys()
	s.waitServes("v2")

	watch := s.watchRequests("v1", "v2")
	withRegistryStopped(t, func() {
		out := s.mustAzud("rollback", "--no-pull")
		if !strings.Contains(out, "The agent holds "+second) {
			t.Errorf("rollback does not say what it holds:\n%s", out)
		}
		s.waitServes("v1")
		watch.stop()
		watch.requireNoFailures()
		s.requireImage(s.roleContainer(), first)
		s.requireDeployed(first, second)

		out = s.mustAzud("status")
		for _, want := range []string{"cannot be reached", first, second, "Held"} {
			if !strings.Contains(out, want) {
				t.Errorf("status with the registry down does not say %q:\n%s", want, out)
			}
		}
	})

	// The tag still names what the rollback left, so nothing changes.
	id := s.inspect("{{.Id}}")
	out := s.mustAzud("sync")
	if !strings.Contains(out, "held until the tag moves") {
		t.Errorf("sync does not say it holds:\n%s", out)
	}
	if after := s.inspect("{{.Id}}"); after != id {
		t.Errorf("a check replaced the container: %s to %s", id, after)
	}
	s.requireDeployed(first, second)

	third := s.release("v3")
	s.mustAzud("sync")
	s.waitServes("v3")
	s.requireDeployed(third, "")
}

func TestSecondRollbackGoesFurtherBack(t *testing.T) {
	s := newService(t, "rollbackback")
	s.fastConfig()
	first := s.release("v1")
	s.deploys()
	second := s.release("v2")
	s.deploys()
	third := s.release("v3")
	s.deploys()
	s.waitServes("v3")

	s.mustAzud("rollback")
	s.waitServes("v2")
	s.requireDeployed(second, third)

	s.mustAzud("rollback")
	s.waitServes("v1")
	s.requireDeployed(first, third)

	out, err := s.azud("rollback")
	if err == nil || !strings.Contains(out, "no earlier release") {
		t.Errorf("a rollback past the oldest = %v:\n%s", err, out)
	}
	s.waitServes("v1")

	// A digest it left behind can be named: it is still stored.
	s.mustAzud("rollback", second)
	s.waitServes("v2")
	s.requireDeployed(second, third)
}

func TestRollbackWithNoPullRefusesAnImageThatIsNotHere(t *testing.T) {
	s := newService(t, "rollbacknopull")
	s.fastConfig()
	s.release("v1")
	s.deploys()
	second := s.release("v2")
	s.deploys()
	s.waitServes("v2")

	first := s.agentState().Applied[1]
	id, err := podman("image", "inspect", "--format", "{{.Id}}", s.repo+":v1")
	if err != nil {
		t.Fatalf("inspect the old image: %v\n%s", err, id)
	}
	if out, err := podman("rmi", "-f", strings.TrimSpace(id)); err != nil {
		t.Fatalf("remove the old image: %v\n%s", err, out)
	}

	out, err := s.azud("rollback", "--no-pull")
	if err == nil || !strings.Contains(out, "is not on this server") {
		t.Fatalf("rollback with no image = %v:\n%s", err, out)
	}
	s.waitServes("v2")
	s.requireDeployed(second, "")

	// Without the flag it fetches the image from the registry.
	s.mustAzud("rollback")
	s.waitServes("v1")
	st := s.agentState()
	if st.Current != first || st.FailedDigest != second {
		t.Errorf("state = %+v", st)
	}
}

func TestSyncDigestVerifiesOnlyWhatItHasNotStored(t *testing.T) {
	s := newService(t, "syncdigest")
	s.fastConfig()
	first := s.release("v1")
	s.deploys()
	second := s.release("v2")
	s.deploys()
	s.waitServes("v2")

	unknown := s.publish("v3")
	out, err := s.azud("sync", "--digest", unknown)
	if err == nil || !strings.Contains(out, "verification") {
		t.Fatalf("sync --digest of an unsigned image = %v:\n%s", err, out)
	}
	s.requireDeployed(second, "")
	s.waitServes("v2")

	s.mustAzud("sync", "--digest", unknown, "--no-verify")
	s.waitServes("v3")
	s.requireDeployed(unknown, second)

	// Stored, so not verified again, and with the registry down.
	if err := os.Remove(filepath.Join(s.signed, first)); err != nil {
		t.Fatal(err)
	}
	withRegistryStopped(t, func() {
		s.mustAzud("sync", "--digest", first)
		s.waitServes("v1")
	})
	s.requireDeployed(first, second)

	out, err = s.azud("sync", "--no-verify")
	if err == nil || !strings.Contains(out, "--digest") {
		t.Errorf("--no-verify alone = %v:\n%s", err, out)
	}
}

func TestRedeployAppliesAnEditedConfiguration(t *testing.T) {
	s := newService(t, "redeployedit")
	s.fastConfig()
	digest := s.release("v1")
	s.deploys()
	s.waitServes("v1")
	before := s.agentState().Current
	if out := s.mustAzud("status"); strings.Contains(out, "edited since") {
		t.Errorf("status says the configuration changed:\n%s", out)
	}

	config := fastStops(s.renderConfig()) + "\nenv:\n  clear:\n    EDITED: applied\n"
	s.writeConfig(config)
	if out := s.mustAzud("status"); !strings.Contains(out, "edited since") || !strings.Contains(out, "azud redeploy") {
		t.Errorf("status does not say the configuration changed:\n%s", out)
	}
	// The next check does not apply it.
	s.deploys()
	if env := s.inspect("{{json .Config.Env}}"); strings.Contains(env, "EDITED") {
		t.Fatalf("a check applied an edit: %s", env)
	}

	watch := s.watchRequests("v1")
	s.mustAzud("redeploy")
	s.waitServes("v1")
	watch.stop()
	watch.requireNoFailures()
	if env := s.inspect("{{json .Config.Env}}"); !strings.Contains(env, "EDITED=applied") {
		t.Errorf("env = %s", env)
	}
	st := s.agentState()
	if st.DeployedDigest != digest || st.Current == before || !strings.HasPrefix(st.Current, strings.TrimPrefix(digest, "sha256:")+"-") || st.Journal != nil {
		t.Errorf("state = %+v; want a new record for the same digest", st)
	}
	if out := s.mustAzud("status"); strings.Contains(out, "edited since") {
		t.Errorf("status still says the configuration changed:\n%s", out)
	}

	// A rollback undoes the edit, from its record, whatever the file says now.
	s.mustAzud("rollback")
	s.waitServes("v1")
	if env := s.inspect("{{json .Config.Env}}"); strings.Contains(env, "EDITED") {
		t.Errorf("env after the rollback = %s", env)
	}
	if got := s.agentState().Current; got != before {
		t.Errorf("current = %s, want %s", got, before)
	}
}

// A running agent read the file before the edit, and must not put back what it read.
func TestARunningAgentKeepsAnEditAcrossTheNextRelease(t *testing.T) {
	s := newService(t, "redeployrunning")
	s.writeConfig(strings.Replace(fastStops(s.renderConfig()), "interval: 1m", "interval: 10s", 1))
	s.release("v1")
	a := s.startAgent()
	a.waitFor("the first release", func() bool {
		got, err := s.serves()
		return err == nil && got == "v1"
	})

	config, err := os.ReadFile(s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.writeConfig(string(config) + "\nenv:\n  clear:\n    EDITED: applied\n")
	s.mustAzud("redeploy")
	if env := s.inspect("{{json .Config.Env}}"); !strings.Contains(env, "EDITED=applied") {
		t.Fatalf("env after the redeploy = %s", env)
	}

	second := s.release("v2")
	a.waitFor("the next release", func() bool {
		got, err := s.serves()
		return err == nil && got == "v2"
	})
	a.stop()
	s.requireDeployed(second, "")
	if env := s.inspect("{{json .Config.Env}}"); !strings.Contains(env, "EDITED=applied") {
		t.Errorf("the next release lost the edit: %s", env)
	}
}

func TestRedeployOfABundleUsesItsRecord(t *testing.T) {
	s := newBundleService(t, "redeploybundle")
	digest := s.publishBundle(s.publish("v1"), "one", fastStops)
	s.promote(digest)
	s.deploys()
	s.waitServes("v1")
	before := s.inspect("{{.Id}}")

	if err := os.WriteFile(s.secretsFile, []byte("APP_SECRET=overwritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withRegistryStopped(t, func() { s.mustAzud("redeploy") })
	s.waitServes("v1")
	if after := s.inspect("{{.Id}}"); after == before {
		t.Error("redeploy did not replace the container")
	}
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	s.requireDeployed(digest, "")
}

// The bad case: an agent killed halfway through a rollback must not return to what the
// rollback left, or forget what it holds.
func TestInterruptedRollbackFinishes(t *testing.T) {
	s := newService(t, "rollbackkilled")
	s.withWorker("sleep 3600")
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")
	second := s.release("v2")
	s.deploys()
	s.waitServes("v2")

	s.killAt("*"+s.workerContainer()+"-new*", "rollback")
	st := s.agentState()
	if st.Journal == nil || st.Journal.Digest != first || st.DeployedDigest != second {
		t.Fatalf("state after the kill = %+v, journal %+v", st, st.Journal)
	}
	s.requireImage(s.roleContainer(), first)
	s.requireImage(s.workerContainer(), second)

	withRegistryStopped(t, func() {
		out := s.mustAzud("status")
		if !strings.Contains(out, "Not finished") || !strings.Contains(out, "finishes it") {
			t.Errorf("status during the interrupted rollback:\n%s", out)
		}
		s.deploys()
	})
	s.waitServes("v1")
	s.requireImage(s.roleContainer(), first)
	s.requireImage(s.workerContainer(), first)
	s.requireDeployed(first, second)
	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("next check = %v, want the tag held:\n%s", err, out)
	}
}

func TestBundleRollbackBringsBackItsSecretsWithTheRegistryDown(t *testing.T) {
	s := newBundleService(t, "rollbackbundle")
	first := s.publishBundle(s.publish("v1"), "one", fastStops)
	s.promote(first)
	s.deploys()
	s.waitServes("v1")
	second := s.publishBundle(s.publish("v2"), "two", fastStops)
	s.promote(second)
	s.deploys()
	s.waitServes("v2")

	withRegistryStopped(t, func() { s.mustAzud("rollback") })
	s.waitServes("v1")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	if env := s.inspect("{{json .Config.Env}}"); !strings.Contains(env, "APP_SECRET=one") {
		t.Errorf("env = %s", env)
	}
	s.requireDeployed(first, second)
}

// Moving a tag alone cannot bring back an old bundle. Publishing it again can, because the
// new digest must be signed.
func TestRollbackStopsARoleTheReleaseItLeftAdded(t *testing.T) {
	s := newBundleService(t, "rollbackrole")
	first := s.publishBundle(s.publish("v1"), "one", fastStops)
	s.promote(first)
	s.deploys()
	s.waitServes("v1")
	withWorker := func(config string) string {
		return fastStops(strings.Replace(config, "      - 127.0.0.1\n", "      - 127.0.0.1\n  worker:\n    hosts:\n      - 127.0.0.1\n    cmd: \"sleep 3600\"\n", 1))
	}
	second := s.publishBundle(s.publish("v2"), "two", withWorker)
	s.promote(second)
	s.deploys()
	s.waitServes("v2")
	if !s.running(s.workerContainer()) {
		t.Fatal("the second release's worker is not running")
	}

	s.mustAzud("rollback")
	s.waitServes("v1")
	if s.running(s.workerContainer()) {
		t.Error("the worker of the release the rollback left is still running")
	}
	s.requireDeployed(first, second)
}

func TestRepublishThenPromoteBringsBackAnOldRelease(t *testing.T) {
	s := newBundleService(t, "republish")
	first := s.publishBundle(s.publish("v1"), "one", fastStops)
	s.promote(first)
	s.deploys()
	s.waitServes("v1")
	second := s.publishBundle(s.publish("v2"), "two", fastStops)
	s.promote(second)
	s.deploys()
	s.waitServes("v2")

	s.promote(first)
	out, err := s.agentOnce()
	if err == nil || !strings.Contains(out, "older than") {
		t.Fatalf("moving the tag to an old bundle = %v:\n%s", err, out)
	}
	s.waitServes("v2")
	if st := s.agentState(); st.DeployedDigest != second {
		t.Fatalf("state = %+v", st)
	}

	// Run from the repository, with no secrets file: the layer is copied as it is.
	repo := filepath.Join(s.dir, "repo", "n")
	if err := os.Remove(filepath.Join(repo, "secrets")); err != nil {
		t.Fatal(err)
	}
	digestFile := filepath.Join(s.dir, "republished")
	command := exec.Command(h.binary, "release", "republish", first, "--digest-file", digestFile, "-c", filepath.Join(repo, "deploy.yml"))
	command.Dir = repo
	command.Env = append(os.Environ(), "SSL_CERT_FILE="+h.caFile)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release republish: %v\n%s", err, out)
	}
	data, err := os.ReadFile(digestFile)
	if err != nil {
		t.Fatal(err)
	}
	republished := strings.TrimSpace(string(data))
	if republished == first {
		t.Fatal("republish kept the digest")
	}

	// Unsigned, it is refused, so it is the signature that authorizes this.
	s.promote(republished)
	if out, err := s.agentOnce(); err == nil || !strings.Contains(out, "verification") {
		t.Fatalf("an unsigned republished bundle = %v:\n%s", err, out)
	}
	s.waitServes("v2")

	s.sign(republished)
	if out, err := s.agentOnce("--retry-failed"); err != nil {
		t.Fatalf("a signed republished bundle: %v\n%s", err, out)
	}
	s.waitServes("v1")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	s.requireDeployed(republished, "")
}

// runningAgent is `azud agent run`, as the unit runs it.
type runningAgent struct {
	s       *service
	command *exec.Cmd
	log     *os.File
	done    chan error
}

func (s *service) startAgent() *runningAgent {
	s.t.Helper()
	logFile, err := os.CreateTemp(s.dir, "agent-*.log")
	if err != nil {
		s.t.Fatal(err)
	}
	command := exec.Command(h.binary, "agent", "run", "-c", s.config)
	command.Dir = s.dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		s.t.Fatal(err)
	}
	a := &runningAgent{s: s, command: command, log: logFile, done: make(chan error, 1)}
	go func() { a.done <- command.Wait() }()
	s.t.Cleanup(func() { _ = a.stop() })
	return a
}

// stop asks the agent to stop, as systemd does, and returns everything it logged. Calling
// it again does nothing more.
func (a *runningAgent) stop() string {
	if a.command != nil {
		_ = a.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-a.done:
		case <-time.After(3 * time.Minute):
			_ = a.command.Process.Kill()
			<-a.done
		}
		a.command = nil
		_ = a.log.Close()
	}
	return a.output()
}

func (a *runningAgent) output() string {
	data, err := os.ReadFile(a.log.Name())
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func (a *runningAgent) waitFor(what string, done func() bool) {
	a.s.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !done() {
		if time.Now().After(deadline) {
			a.s.t.Fatalf("%s did not happen; the agent logged:\n%s", what, a.output())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (a *runningAgent) waitLogged(text string) {
	a.s.t.Helper()
	a.waitFor("logging "+text, func() bool { return strings.Contains(a.output(), text) })
}

func (a *runningAgent) waitRunning(container string) {
	a.s.t.Helper()
	a.waitFor(container+" running", func() bool { return a.s.running(container) })
}

// A container that exits at once on SIGTERM, so stopping it in a test takes no time.
const stopsAtOnce = `trap 'exit 0' TERM; while :; do sleep 1; done`

// withEverything is a service with every kind of container an agent restores.
func (s *service) withEverything() {
	s.t.Helper()
	config := strings.Replace(s.renderConfig(), "      - 127.0.0.1\n", "      - 127.0.0.1\n  worker:\n    hosts:\n      - 127.0.0.1\n    cmd: \"sleep 3600\"\n", 1)
	s.writeConfig(fastStops(config) + fmt.Sprintf(`
accessories:
  db:
    image: docker.io/library/busybox:1.37
    host: 127.0.0.1
    cmd: %[1]q
  cache:
    image: docker.io/library/busybox:1.37
    host: 127.0.0.1
    cmd: %[1]q

cron:
  sweep:
    schedule: "0 0 1 1 *"
    command: "true"
`, stopsAtOnce))
}

// stopAsAtReboot stops containers without azud knowing, which is what a reboot does to them.
func (s *service) stopAsAtReboot(containers ...string) {
	s.t.Helper()
	for _, container := range containers {
		if out, err := podman("stop", "-t", "1", container); err != nil {
			s.t.Fatalf("stop %s: %v\n%s", container, err, out)
		}
	}
}

func removeProxy(t *testing.T) {
	t.Helper()
	var out string
	var err error
	if h.proxyRootful && !h.root {
		out, err = run("sudo", "-n", "podman", "rm", "-f", proxyContainer)
	} else {
		out, err = podman("rm", "-f", proxyContainer)
	}
	if err != nil {
		t.Fatalf("remove the proxy: %v\n%s", err, out)
	}
}

func TestRestartRestoresEverythingWithoutTheRegistry(t *testing.T) {
	s := newService(t, "restart")
	s.withEverything()
	digest := s.releaseCronImage("v1")
	s.deploys()
	s.waitServes("v1")
	db, cache, cron := s.name+"-db", s.name+"-cache", s.cronContainer("sweep")
	s.mustAzud("accessory", "boot", "db")
	s.mustAzud("accessory", "boot", "cache")
	s.mustAzud("cron", "boot", "sweep")
	s.mustAzud("accessory", "stop", "cache")
	if s.running(cache) {
		t.Fatal("the stopped accessory is running")
	}
	dbID := s.containerID(db)
	workerID := s.containerID(s.workerContainer())
	cronID := s.containerID(cron)

	// A reboot leaves containers stopped. The web container and the proxy are removed as
	// well, which is the harder case: they are created again from the record.
	s.stopAsAtReboot(s.workerContainer(), db, cron)
	if out, err := podman("rm", "-f", s.roleContainer()); err != nil {
		t.Fatalf("remove the web role: %v\n%s", err, out)
	}
	removeProxy(t)

	withRegistryStopped(t, func() {
		a := s.startAgent()
		a.waitFor("the app serving", func() bool {
			got, err := s.serves()
			return err == nil && got == "v1"
		})
		a.waitRunning(s.workerContainer())
		a.waitRunning(db)
		a.waitRunning(cron)
		a.waitLogged("An operator stopped " + cache)
		if s.running(cache) {
			t.Errorf("an accessory an operator stopped was started:\n%s", a.output())
		}
		s.requireImage(s.roleContainer(), digest)
		for container, id := range map[string]string{db: dbID, s.workerContainer(): workerID, cron: cronID} {
			if after := s.containerID(container); after != id {
				t.Errorf("%s was replaced: %s to %s", container, id, after)
			}
		}
		out := a.stop()
		if strings.Contains(out, "Restoring this server") {
			t.Errorf("the restore reported a failure:\n%s", out)
		}
		s.requireDeployed(digest, "")

		status := s.mustAzud("status", "--details")
		for _, want := range []string{"Stopped accessories", "cache", s.workerContainer(), "role worker", cron} {
			if !strings.Contains(status, want) {
				t.Errorf("status does not say %q:\n%s", want, status)
			}
		}
	})

	// Booting it ends the stop, so the next restart starts it.
	s.mustAzud("accessory", "boot", "cache")
	s.stopAsAtReboot(cache)
	a := s.startAgent()
	a.waitRunning(cache)
	a.stop()
}

func TestAnAppAnOperatorStoppedStaysStopped(t *testing.T) {
	s := newService(t, "operatorstop")
	s.fastConfig()
	s.release("v1")
	s.deploys()
	s.waitServes("v1")

	s.mustAzud("app", "stop")
	if s.roleRunning() {
		t.Fatal("the app is running after app stop")
	}
	a := s.startAgent()
	a.waitLogged("leaving it stopped")
	time.Sleep(2 * time.Second)
	if s.roleRunning() {
		t.Errorf("the app an operator stopped was started:\n%s", a.output())
	}
	if out := s.mustAzud("status"); !strings.Contains(out, "Stopped roles") {
		t.Errorf("status does not say the role is stopped:\n%s", out)
	}
	a.stop()

	s.mustAzud("app", "start")
	s.waitServes("v1")
	if out := s.mustAzud("status"); strings.Contains(out, "Stopped roles") {
		t.Errorf("status still says the role is stopped:\n%s", out)
	}
	s.stopAsAtReboot(s.roleContainer())
	a = s.startAgent()
	a.waitFor("the app serving", func() bool {
		got, err := s.serves()
		return err == nil && got == "v1"
	})
	a.stop()

	// A deploy starts the app, and ends the stop.
	s.mustAzud("app", "stop")
	s.release("v2")
	s.mustAzud("sync")
	s.waitServes("v2")
	if out := s.mustAzud("status"); strings.Contains(out, "Stopped roles") {
		t.Errorf("status says a deployed role is stopped:\n%s", out)
	}
}

func TestRestartFinishesAnOpenJournalWithoutTheRegistry(t *testing.T) {
	s := newService(t, "restartjournal")
	s.withWorker("sleep 3600")
	s.release("v1")
	s.deploys()
	s.waitServes("v1")
	second := s.release("v2")
	s.killAgentAt("*" + s.workerContainer() + "-new*")
	if st := s.agentState(); st.Journal == nil {
		t.Fatalf("state after the kill = %+v", st)
	}

	withRegistryStopped(t, func() {
		a := s.startAgent()
		a.waitFor("the journal closing", func() bool { return s.agentState().Journal == nil })
		a.waitLogged("Resuming the interrupted deploy")
		a.stop()
	})
	s.waitServes("v2")
	s.requireImage(s.workerContainer(), second)
	s.requireDeployed(second, "")
}

// The second release's secrets went into the file before the kill. Restoring must not
// create the first release's container with them, and the undo puts them back.
func TestRestartWithABundleKeepsSecretsAndImageTogether(t *testing.T) {
	s := newBundleService(t, "restartbundle")
	first := s.publishBundle(s.publish("v1"), "one", fastStops)
	s.promote(first)
	s.deploys()
	s.waitServes("v1")

	second := s.publishBundle(s.publish("v2"), "two", fastStops)
	s.promote(second)
	s.killAgentAt("*" + s.roleContainer() + "-new*")
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=two") {
		t.Fatalf("runtime secrets after the kill = %q", got)
	}
	// The tag moved back, so the journal undoes.
	s.promote(first)
	s.editState(func(st map[string]any) { st["last_seen_digest"] = first })

	if out, err := podman("rm", "-f", s.roleContainer()); err != nil {
		t.Fatalf("remove the web role: %v\n%s", err, out)
	}
	withRegistryStopped(t, func() {
		a := s.startAgent()
		a.waitFor("the app serving", func() bool {
			got, err := s.serves()
			return err == nil && got == "v1"
		})
		a.stop()
	})
	if env := s.inspect("{{json .Config.Env}}"); !strings.Contains(env, "APP_SECRET=one") {
		t.Errorf("env = %s", env)
	}
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets = %q", got)
	}
	if st := s.agentState(); st.DeployedDigest != first || st.Journal != nil {
		t.Errorf("state = %+v, journal %+v", st, st.Journal)
	}
}

func TestRestoreLeavesADeployCommandsContainers(t *testing.T) {
	s := newService(t, "restartcommand")
	ran := filepath.Join(s.dir, "ran")
	config := strings.Replace(s.renderConfig(), "agent:\n", fmt.Sprintf(`agent:
  deploy_command: |-
    echo "$$AZUD_AGENT_DIGEST" >> %s
`, ran), 1)
	s.writeConfig(config)
	digest := s.release("v1")
	s.deploys()

	// A container with the role's name that the command is responsible for.
	if out, err := podman("run", "-d", "--name", s.roleContainer(), "--label", "azud.service="+s.name, "docker.io/library/busybox:1.37", "sleep", "3600"); err != nil {
		t.Fatalf("start the command's container: %v\n%s", err, out)
	}
	s.stopAsAtReboot(s.roleContainer())

	a := s.startAgent()
	a.waitLogged("owns this service's containers")
	time.Sleep(2 * time.Second)
	if s.roleRunning() {
		t.Errorf("the agent started a container the command owns:\n%s", a.output())
	}
	out := a.stop()
	data, err := os.ReadFile(ran)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != digest {
		t.Errorf("deploy_command ran for %v, want once, before the restart:\n%s", got, out)
	}

	// Going back runs the command for the digest it names, and holds the tag.
	second := s.release("v2")
	s.mustAzud("sync")
	s.mustAzud("rollback")
	if data, err = os.ReadFile(ran); err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 3 || got[2] != digest {
		t.Errorf("deploy_command ran for %v", got)
	}
	s.requireDeployed(digest, second)
}

func TestStatusSaysWhatIsRunningAndWhatDiffers(t *testing.T) {
	s := newService(t, "statusdrift")
	s.withEverything()
	if out := s.mustAzud("status"); !strings.Contains(out, "nothing yet") {
		t.Errorf("status before the first deploy:\n%s", out)
	}
	first := s.releaseCronImage("v1")
	s.deploys()
	s.waitServes("v1")
	s.mustAzud("accessory", "boot", "db")

	out := s.mustAzud("status")
	for _, want := range []string{first, "Since", "Next", "nothing: the tag names what is deployed", "cache", "missing", "azud accessory boot cache", "sweep", "azud cron boot sweep"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Containers:") {
		t.Errorf("status shows containers without --details:\n%s", out)
	}
	if out := s.mustAzud("status", "--details"); !strings.Contains(out, "Containers:") || !strings.Contains(out, s.name+"-db") {
		t.Errorf("status --details:\n%s", out)
	}

	bad := s.publishBroken("v2")
	s.sign(bad)
	s.promote(bad)
	if out, err := s.agentOnce(); err == nil {
		t.Fatalf("agent accepted a broken release:\n%s", out)
	}
	out = s.mustAzud("status")
	for _, want := range []string{"Failed", bad, "Last error", "readiness", "azud sync --retry"} {
		if !strings.Contains(out, want) {
			t.Errorf("status after a failure does not say %q:\n%s", want, out)
		}
	}
	// It changes nothing.
	before, err := os.ReadFile(filepath.Join(s.state, "agent", s.name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	s.mustAzud("status", "--details")
	after, err := os.ReadFile(filepath.Join(s.state, "agent", s.name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("status changed the state:\n%s\n%s", before, after)
	}
}
