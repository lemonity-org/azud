//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cronImage serves on 8080 like every app here, and has what busybox's crond needs.
const cronImage = "RUN mkdir -p /var/spool/cron/crontabs\n" + `CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]`

// azud runs a command against the service's config, as an operator on the server would.
func (s *service) azud(args ...string) (string, error) {
	command := exec.Command(h.binary, append(args, "-c", s.config)...)
	command.Dir = s.dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile)
	out, err := command.CombinedOutput()
	return string(out), err
}

func (s *service) mustAzud(args ...string) string {
	s.t.Helper()
	out, err := s.azud(args...)
	if err != nil {
		s.t.Fatalf("azud %v: %v\n%s", args, err, out)
	}
	return out
}

func (s *service) releaseCronImage(version string) string {
	s.t.Helper()
	digest := s.publishImage(version, cronImage)
	s.sign(digest)
	s.promote(digest)
	return digest
}

func (s *service) cronContainer(name string) string { return s.name + "-cron-" + name }

func (s *service) containerID(container string) string {
	s.t.Helper()
	out, err := podman("container", "inspect", "--format", "{{.Id}}", container)
	if err != nil {
		s.t.Fatalf("inspect %s: %v\n%s", container, err, out)
	}
	return strings.TrimSpace(out)
}

// withCronJobs makes the config name configured as its image, a tag the agent does not
// follow, and adds a job that copies what the image serves to a file in out each minute,
// and one a test runs by hand.
func (s *service) withCronJobs(configured, out string) {
	s.t.Helper()
	config := strings.Replace(s.renderConfig(), "image: "+s.repo+"\n", "image: "+configured+"\n", 1)
	config = fastStops(config)
	config += fmt.Sprintf(`
volumes:
  - %s:/out

cron:
  scheduled:
    schedule: "* * * * *"
    command: cp /www/index.html /out/scheduled
  manual:
    schedule: "0 0 1 1 *"
    command: cp /www/index.html /out/manual
`, out)
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func TestCronJobsRunTheDeployedDigestNotTheConfiguredTag(t *testing.T) {
	s := newService(t, "cronpin")
	out := filepath.Join(s.dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// The configured tag exists, so running it is a real mistake and not a failed pull.
	s.publishImage("configured", cronImage)
	s.withCronJobs(s.repo+":configured", out)

	// Before the first deploy there is no release to run, and reading still works.
	got, err := s.azud("cron", "boot")
	if err == nil {
		t.Fatalf("cron boot ran with nothing deployed:\n%s", got)
	}
	for _, want := range []string{"nothing is deployed", "azud agent run --once"} {
		if !strings.Contains(got, want) {
			t.Errorf("cron boot does not say %q:\n%s", want, got)
		}
	}
	if got := s.mustAzud("cron", "list"); !strings.Contains(got, "Nothing is deployed") {
		t.Errorf("cron list does not say nothing is deployed:\n%s", got)
	}

	first := s.releaseCronImage("v1")
	s.deploys()
	s.waitServes("v1")

	s.mustAzud("cron", "boot")
	if got, want := s.imageOf(s.cronContainer("scheduled")), s.repo+"@"+first; got != want {
		t.Errorf("the scheduled job runs %q, want %q", got, want)
	}
	if got := s.inspectLabel(s.cronContainer("scheduled"), "azud.spec"); got == "" {
		t.Error("the scheduled job has no azud.spec label")
	}

	// Each job does what it says from the deployed image's filesystem.
	s.mustAzud("cron", "run", "manual")
	if got := readTrimmed(filepath.Join(out, "manual")); got != "v1" {
		t.Errorf("a manual run read %q, want v1 and not the configured tag's content", got)
	}
	deadline := time.Now().Add(90 * time.Second)
	for readTrimmed(filepath.Join(out, "scheduled")) != "v1" && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if got := readTrimmed(filepath.Join(out, "scheduled")); got != "v1" {
		logs, _ := podman("logs", s.cronContainer("scheduled"))
		t.Errorf("the schedule read %q, want v1; logs:\n%s", got, logs)
	}

	// Booting again leaves a job alone while it matches the release.
	id := s.containerID(s.cronContainer("scheduled"))
	if got := s.mustAzud("cron", "boot"); !strings.Contains(got, "already running") {
		t.Errorf("a second cron boot did not leave the job alone:\n%s", got)
	}
	if after := s.containerID(s.cronContainer("scheduled")); after != id {
		t.Errorf("a second cron boot replaced the job: %s to %s", id, after)
	}

	// A deploy leaves cron jobs as they are, and boot brings them to the new release.
	second := s.releaseCronImage("v2")
	s.deploys()
	s.waitServes("v2")
	if got, want := s.imageOf(s.cronContainer("scheduled")), s.repo+"@"+first; got != want {
		t.Errorf("a deploy changed the scheduled job to %q", got)
	}
	if got := s.mustAzud("cron", "boot"); !strings.Contains(got, "differs from the release") {
		t.Errorf("cron boot did not recreate a job on the old image:\n%s", got)
	}
	if got, want := s.imageOf(s.cronContainer("scheduled")), s.repo+"@"+second; got != want {
		t.Errorf("after boot, the scheduled job runs %q, want %q", got, want)
	}
	s.mustAzud("cron", "run", "manual")
	if got := readTrimmed(filepath.Join(out, "manual")); got != "v2" {
		t.Errorf("a manual run read %q, want v2", got)
	}
}

func TestWritingFromTheReleaseIsRefusedWhileAJournalIsOpen(t *testing.T) {
	s := newService(t, "cronsettling")
	out := filepath.Join(s.dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	s.withCronJobs(s.repo, out)
	first := s.releaseCronImage("v1")
	s.deploys()
	s.waitServes("v1")
	s.mustAzud("cron", "boot")
	id := s.containerID(s.cronContainer("scheduled"))

	second := s.releaseCronImage("v2")
	s.killAgentAt("*" + s.roleContainer() + "-new*")
	st := s.agentState()
	if st.Journal == nil || st.Journal.Digest != second || st.DeployedDigest != first {
		t.Fatalf("state after the kill = %+v, journal %+v", st, st.Journal)
	}

	for _, args := range [][]string{{"cron", "boot"}, {"cron", "boot", "manual"}, {"proxy", "reconcile", "--repair"}} {
		got, err := s.azud(args...)
		if err == nil {
			t.Errorf("azud %v ran with a journal open:\n%s", args, got)
		}
		for _, want := range []string{"has not finished", "azud agent run --once"} {
			if !strings.Contains(got, want) {
				t.Errorf("azud %v does not say %q:\n%s", args, want, got)
			}
		}
	}
	if after := s.containerID(s.cronContainer("scheduled")); after != id {
		t.Errorf("a refused cron boot replaced the job: %s to %s", id, after)
	}

	// What reads, or runs and leaves nothing, still works, on the last whole release.
	if got := s.mustAzud("cron", "list"); !strings.Contains(got, "scheduled") {
		t.Errorf("cron list:\n%s", got)
	}
	s.mustAzud("proxy", "reconcile", "--check")
	s.mustAzud("cron", "run", "manual")
	if got := readTrimmed(filepath.Join(out, "manual")); got != "v1" {
		t.Errorf("a manual run during a deploy read %q, want the last whole release, v1", got)
	}

	s.deploys()
	s.waitServes("v2")
	if got := s.agentState().Journal; got != nil {
		t.Fatalf("journal still open: %+v", got)
	}
	s.mustAzud("cron", "boot")
	if got, want := s.imageOf(s.cronContainer("scheduled")), s.repo+"@"+second; got != want {
		t.Errorf("after the journal closed, the job runs %q, want %q", got, want)
	}
}

// A script's own journal is open around it. Refusing it would fail the release, and
// it means the release it is deploying.
func TestDeployCommandCanRepairTheProxyForItsOwnRelease(t *testing.T) {
	s := newService(t, "commandrepair")
	result := filepath.Join(s.dir, "repair-result")
	config := strings.Replace(s.renderConfig(), "agent:\n", fmt.Sprintf(`agent:
  deploy_command: |-
    test -n "$$AZUD_AGENT_IMAGE"
    %[1]s proxy reconcile --repair -c %[2]s >> %[3]s 2>&1
    echo "repair=$$?" >> %[3]s
`, h.binary, s.config, result), 1)
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, version := range []string{"v1", "v2"} {
		s.release(version)
		s.deploys()
		data, err := os.ReadFile(result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(data), "repair=0\n") {
			t.Fatalf("azud proxy reconcile --repair inside the script of %s:\n%s", version, data)
		}
	}
}

const accessoryInRelease = `
accessories:
  db:
    image: docker.io/library/busybox:1.37
    host: 127.0.0.1
    cmd: sleep 3600
    env:
      secret: [APP_SECRET]
  elsewhere:
    image: docker.io/library/busybox:1.37
    host: 10.9.9.9
    cmd: sleep 3600
`

func TestAccessoryBootsFromAReleaseBeforeTheFirstDeploy(t *testing.T) {
	s := newBundleService(t, "accrelease")
	withAccessory := func(config string) string { return config + accessoryInRelease }
	bundle := s.publishBundle(s.publish("v1"), "one", withAccessory)
	s.promote(bundle)
	db := s.name + "-db"

	got, err := s.azud("accessory", "boot", "db")
	if err == nil || !strings.Contains(got, "--release <tag>") {
		t.Errorf("a plain boot with nothing deployed = %v, want it to name --release:\n%s", err, got)
	}

	// Verification is what makes the release's spec and secrets worth using.
	signature := filepath.Join(s.signed, bundle)
	if err := os.Remove(signature); err != nil {
		t.Fatal(err)
	}
	got, err = s.azud("accessory", "boot", "db", "--release", trackTag)
	if err == nil || !strings.Contains(got, "verification") {
		t.Errorf("an unverified release = %v:\n%s", err, got)
	}
	if s.running(db) {
		t.Fatal("an unverified release started an accessory")
	}
	if err := os.WriteFile(signature, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The release places this one elsewhere, so an empty database must not start here.
	got, err = s.azud("accessory", "boot", "elsewhere", "--release", trackTag)
	if err == nil || !strings.Contains(got, "not on this server") {
		t.Errorf("an accessory placed elsewhere = %v:\n%s", err, got)
	}
	if out, err := podman("container", "exists", s.name+"-elsewhere"); err == nil {
		t.Errorf("an accessory placed elsewhere started here:\n%s", out)
	}

	s.mustAzud("accessory", "boot", "db", "--release", trackTag)
	if !s.running(db) {
		t.Fatal("the accessory is not running")
	}
	env, err := podman("container", "inspect", "--format", "{{json .Config.Env}}", db)
	if err != nil {
		t.Fatalf("inspect %s: %v\n%s", db, err, env)
	}
	if !strings.Contains(env, "APP_SECRET=one") {
		t.Errorf("the accessory did not get the release's secret: %s", env)
	}
	if got := s.inspectLabel(db, "azud.spec"); got == "" {
		t.Error("the accessory has no azud.spec label")
	}

	// It applied nothing else, so a first deploy finds this server as it was.
	for _, path := range []string{
		filepath.Join(s.state, "agent", s.name+".json"),
		filepath.Join(s.state, "agent", s.name, "releases"),
		s.secretsFile,
		s.secretsFile + ".pre-bundle",
		s.secretsFile + ".pre-bundle-absent",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after booting an accessory: %v", path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(s.secretsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("%s was left beside the runtime secrets file", entry.Name())
	}
	if got := s.roleRunning(); got {
		t.Error("booting an accessory started the application")
	}

	// Booting again changes nothing, and the deploy leaves the accessory running.
	id := s.containerID(db)
	s.mustAzud("accessory", "boot", "db", "--release", trackTag)
	s.deploys()
	s.waitServes("v1")
	if after := s.containerID(db); after != id {
		t.Errorf("the accessory was replaced: %s to %s", id, after)
	}
	if st := s.agentState(); st.DeployedDigest != bundle || st.Journal != nil {
		t.Errorf("state after the first deploy = %+v, journal %+v", st, st.Journal)
	}
	if got := s.runtimeSecrets(); !strings.Contains(got, "APP_SECRET=one") {
		t.Errorf("runtime secrets after the first deploy = %q", got)
	}
}

func TestAccessoryBootWithAReleaseIsRefusedInImageMode(t *testing.T) {
	s := newService(t, "accimage")
	s.release("v1")
	got, err := s.azud("accessory", "boot", "db", "--release", trackTag)
	if err == nil {
		t.Fatalf("--release ran in image mode:\n%s", got)
	}
	for _, want := range []string{"agent.source: bundle", "azud accessory boot db"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got)
		}
	}
}

func (s *service) inspectLabel(container, label string) string {
	s.t.Helper()
	out, err := podman("container", "inspect", "--format", `{{index .Config.Labels "`+label+`"}}`, container)
	if err != nil {
		s.t.Fatalf("inspect %s: %v\n%s", container, err, out)
	}
	return strings.TrimSpace(out)
}

func (s *service) roleRunning() bool {
	out, err := podman("inspect", "--format", "{{.State.Running}}", s.roleContainer())
	return err == nil && strings.TrimSpace(out) == "true"
}
