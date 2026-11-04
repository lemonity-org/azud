package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/localtest"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
)

const operatorDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const noAgentConfig = `service: app
image: ghcr.io/acme/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
`

const agentSection = `agent:
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
`

const agentConfig = noAgentConfig + agentSection

// useConfig writes a configuration and points the CLI's globals at it, as the root
// pre-run would.
func useConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	savedCfg, savedPath, savedDest := cfg, configPath, destination
	t.Cleanup(func() { cfg, configPath, destination = savedCfg, savedPath, savedDest })
	configPath, destination = filepath.Join(dir, "deploy.yml"), ""
	loaded, err := config.NewLoader(configPath, "").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg = loaded
	return dir
}

func TestRollbackArguments(t *testing.T) {
	const wantOne = "accepts 1 arg(s), received 0"

	t.Run("no agent section still needs a version", func(t *testing.T) {
		useConfig(t, map[string]string{"deploy.yml": noAgentConfig})
		if err := rollbackArgs(rollbackCmd, nil); err == nil || err.Error() != wantOne {
			t.Fatalf("rollbackArgs = %v", err)
		}
		if err := rollbackArgs(rollbackCmd, []string{"v1"}); err != nil {
			t.Fatalf("rollbackArgs(v1) = %v", err)
		}
	})
	t.Run("no configuration fails as it did", func(t *testing.T) {
		useConfig(t, map[string]string{"deploy.yml": noAgentConfig})
		configPath = filepath.Join(t.TempDir(), "missing.yml")
		if err := rollbackArgs(rollbackCmd, nil); err == nil || err.Error() != wantOne {
			t.Fatalf("rollbackArgs = %v", err)
		}
	})
	t.Run("an agent section takes none", func(t *testing.T) {
		useConfig(t, map[string]string{"deploy.yml": agentConfig})
		if err := rollbackArgs(rollbackCmd, nil); err != nil {
			t.Fatalf("rollbackArgs = %v", err)
		}
	})
	t.Run("an agent section in the destination file counts", func(t *testing.T) {
		useConfig(t, map[string]string{"deploy.yml": noAgentConfig, "deploy.agent.yml": agentSection})
		if err := rollbackArgs(rollbackCmd, nil); err == nil {
			t.Fatal("rollbackArgs ignored the missing destination")
		}
		destination = "agent"
		if err := rollbackArgs(rollbackCmd, nil); err != nil {
			t.Fatalf("rollbackArgs with the destination = %v", err)
		}
	})
	t.Run("two are refused either way", func(t *testing.T) {
		useConfig(t, map[string]string{"deploy.yml": agentConfig})
		if err := rollbackArgs(rollbackCmd, []string{"a", "b"}); err == nil {
			t.Fatal("rollbackArgs took two")
		}
	})
}

func TestNoPullIsRefusedWithoutAnAgentSection(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": noAgentConfig})
	rollbackNoPull = true
	t.Cleanup(func() { rollbackNoPull = false })
	err := runRollback(testCommand(), []string{"v1"})
	if err == nil || !strings.Contains(err.Error(), "--no-pull") {
		t.Fatalf("runRollback = %v", err)
	}
}

func TestPushFlagsAreRefusedOnAnAgentServer(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": agentConfig})
	deployHost = "app-1"
	t.Cleanup(func() { deployHost, deployRole = "", "" })
	if err := runRollback(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("runRollback --host = %v", err)
	}
	if err := runRedeploy(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("runRedeploy --host = %v", err)
	}
	deployHost, deployRole = "", "web"
	if err := runRedeploy(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "--role") {
		t.Errorf("runRedeploy --role = %v", err)
	}
}

func TestSyncRefusesWhatDoesNotApply(t *testing.T) {
	defer func() { syncRetry, syncDigest, syncNoVerify = false, "", false }()
	cases := []struct {
		name, config, want string
		retry              bool
		digest             string
		noVerify           bool
	}{
		{name: "no agent section", config: noAgentConfig, want: "no agent section"},
		{name: "--no-verify alone", config: agentConfig, noVerify: true, want: "--no-verify requires --digest"},
		{name: "--retry with --digest", config: agentConfig, retry: true, digest: operatorDigest, want: "use one of them"},
	}
	for _, c := range cases {
		useConfig(t, map[string]string{"deploy.yml": c.config})
		syncRetry, syncDigest, syncNoVerify = c.retry, c.digest, c.noVerify
		if err := runSync(testCommand(), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: runSync = %v", c.name, err)
		}
	}
}

func TestWhatTakesTheStateLockRefusesUnderIt(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": agentConfig})
	for _, variable := range []string{"AZUD_AGENT_DIGEST", "AZUD_HOOK"} {
		t.Setenv("AZUD_AGENT_DIGEST", "")
		t.Setenv("AZUD_HOOK", "")
		t.Setenv(variable, "set")
		for name, run := range map[string]func() error{
			"sync":      func() error { return runSync(testCommand(), nil) },
			"rollback":  func() error { return runRollback(testCommand(), nil) },
			"redeploy":  func() error { return runRedeploy(testCommand(), nil) },
			"agent run": func() error { return runAgentRun(testCommand(), nil) },
		} {
			if err := run(); err == nil || !strings.Contains(err.Error(), "wait for itself") {
				t.Errorf("%s with %s set = %v", name, variable, err)
			}
		}
	}
}

func TestStatusNeedsAnAgentSection(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": noAgentConfig})
	if err := runStatus(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "no agent section") {
		t.Fatalf("runStatus = %v", err)
	}
}

// statusOutput runs azud status against state saved under a temporary directory, with a
// registry that answers as told.
func statusOutput(t *testing.T, config string, st agent.State, respond func(*http.Request) (*http.Response, error)) string {
	t.Helper()
	useConfig(t, map[string]string{"deploy.yml": config})
	stateDir := t.TempDir()
	t.Setenv("AZUD_STATE_DIR", stateDir)
	store := agent.NewStore(stateDir, "app")
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	savedLogger, savedResolver := output.DefaultLogger, newRegistryResolver
	t.Cleanup(func() { output.DefaultLogger, newRegistryResolver = savedLogger, savedResolver })
	var out bytes.Buffer
	output.DefaultLogger = output.NewLogger(&out, &out, false)
	newRegistryResolver = func() *registry.Resolver {
		return &registry.Resolver{Client: &http.Client{Transport: roundTripFunc(respond)}}
	}
	if err := runStatus(testCommand(), nil); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	return out.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStatusSaysTheRegistryCannotBeReachedAndShowsTheRest(t *testing.T) {
	got := statusOutput(t, agentConfig, agent.State{
		DeployedDigest: operatorDigest,
		LastSeenDigest: operatorDigest,
		FailedDigest:   "sha256:bad",
		LastError:      "deploy of bad failed: health check failed\nmore detail",
		Journal: &agent.Journal{
			Target: "b-id", Digest: "sha256:bbb", Previous: "a-id", PreviousDigest: operatorDigest, Phase: agent.PhaseForward,
		},
	}, func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })

	for _, want := range []string{
		"cannot be reached", "connection refused",
		operatorDigest, // what is deployed, and what the tag last named
		"health check failed",
		"Not finished", "sha256:bbb",
		"Next",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "more detail") {
		t.Errorf("status repeats a whole error:\n%s", got)
	}
}

// Secrets are for the registry password and nothing else, so a command that fails costs
// the line about the tag, and one nothing needs does not run.
func TestStatusLoadsSecretsOnlyForTheRegistryPassword(t *testing.T) {
	unreachable := func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }
	ran := filepath.Join(t.TempDir(), "ran")
	commandConfig := func(extra string) string {
		return strings.Replace(agentConfig, "agent:\n", "secrets_provider: command\nsecrets_command: touch "+ran+"; exit 1\n"+extra+"agent:\n", 1)
	}

	got := statusOutput(t, commandConfig(""), agent.State{DeployedDigest: operatorDigest}, unreachable)
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("secrets_command ran for a registry with no password: %v", err)
	}
	if !strings.Contains(got, operatorDigest) {
		t.Errorf("status:\n%s", got)
	}

	withPassword := "registry:\n  server: ghcr.io\n  username: deploy\n  password: [STATUS_TEST_REGISTRY_TOKEN]\n"
	got = statusOutput(t, commandConfig(withPassword), agent.State{DeployedDigest: operatorDigest}, unreachable)
	if _, err := os.Stat(ran); err != nil {
		t.Errorf("secrets_command did not run for the registry password: %v", err)
	}
	if !strings.Contains(got, "failed to load secrets") || !strings.Contains(got, operatorDigest) {
		t.Errorf("status with a failing secrets_command:\n%s", got)
	}
}

func TestRecordingIntentDoesNothingWithoutAnAgentSection(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": noAgentConfig})
	stateDir := t.TempDir()
	t.Setenv("AZUD_STATE_DIR", stateDir)
	if err := recordRolesStopped(); err != nil {
		t.Fatal(err)
	}
	if err := recordAccessoryStopped("db"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		t.Fatalf("state directory = %v, %v; a push server has no agent to tell", entries, err)
	}
}

func TestStopAndStartRecordIntentForTheSelectedRoles(t *testing.T) {
	useConfig(t, map[string]string{"deploy.yml": strings.Replace(agentConfig, "  web:\n    hosts: [app-1]\n", "  web:\n    hosts: [app-1]\n  worker:\n    hosts: [app-1]\n    cmd: work\n", 1)})
	stateDir := t.TempDir()
	t.Setenv("AZUD_STATE_DIR", stateDir)
	store := agent.NewStore(stateDir, "app")
	t.Cleanup(func() { appRole = "" })

	intent := func() agent.Intent {
		t.Helper()
		got, err := store.Intent()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	appRole = "not-a-role"
	if err := recordRolesStopped(); err != nil {
		t.Fatal(err)
	}
	if got := intent(); len(got.Roles) != 0 {
		t.Fatalf("stopped = %v; the command refuses a role that does not exist", got.Roles)
	}
	appRole = ""
	if err := recordRolesStopped(); err != nil {
		t.Fatal(err)
	}
	if got := intent(); !slices.Equal(got.Roles, []string{"web", "worker"}) {
		t.Fatalf("stopped = %v", got.Roles)
	}
	appRole = "web"
	if err := recordRolesStarted(); err != nil {
		t.Fatal(err)
	}
	if got := intent(); !slices.Equal(got.Roles, []string{"worker"}) {
		t.Fatalf("stopped = %v", got.Roles)
	}
	if err := recordAccessoryStopped("db"); err != nil {
		t.Fatal(err)
	}
	if got := intent(); !slices.Equal(got.Accessories, []string{"db"}) {
		t.Fatalf("stopped = %v", got.Accessories)
	}
	if err := recordAccessoryStarted("db"); err != nil {
		t.Fatal(err)
	}
	if got := intent(); len(got.Accessories) != 0 {
		t.Fatalf("stopped = %v", got.Accessories)
	}
}

func TestStartExistingStartsOnlyWhatItShould(t *testing.T) {
	const listing = `id1|app-db|busybox|Exited|exited|||{"azud.service":"app","azud.accessory":"db"}` + "\n" +
		`id2|app-cache|busybox|Exited|exited|||{"azud.service":"app","azud.accessory":"cache"}` + "\n" +
		`id3|app-queue|busybox|Up|running|||{"azud.service":"app","azud.accessory":"queue"}` + "\n" +
		`id4|app-cron-sweep|busybox|Exited|exited|||{"azud.service":"app","azud.cron":"sweep"}` + "\n" +
		`id5|app-cron-sweep-run|busybox|Exited|exited|||{"azud.service":"app","azud.cron":"sweep","azud.cron.run":"manual"}` + "\n" +
		`id6|app|busybox|Exited|exited|||{"azud.service":"app","azud.role":"web"}` + "\n" +
		`id7|app-paused|busybox|Paused|paused|||{"azud.service":"app","azud.accessory":"paused"}`
	listing2 := strings.ReplaceAll(listing, "|||", "||")
	fake := &localtest.Fake{Handle: func(cmd string, _ io.Reader) (string, string, int) {
		if strings.Contains(cmd, " ps ") {
			return listing2, "", 0
		}
		return "", "", 0
	}}
	start := startExisting(podman.NewContainerManager(podman.NewClient(fake)), "app", "host", output.NewLogger(io.Discard, io.Discard, false))

	started := func() []string {
		var names []string
		for _, cmd := range fake.Commands() {
			if strings.Contains(cmd, " start ") {
				names = append(names, cmd[strings.LastIndex(cmd, " ")+1:])
			}
		}
		return names
	}

	if err := start(agent.DriftAccessory, []string{"cache"}); err != nil {
		t.Fatal(err)
	}
	if got := started(); !slices.Equal(got, []string{"app-db"}) {
		t.Fatalf("started = %v, commands %q", got, fake.Commands())
	}
	if err := start(agent.DriftCron, nil); err != nil {
		t.Fatal(err)
	}
	if got := started(); !slices.Equal(got, []string{"app-db", "app-cron-sweep"}) {
		t.Fatalf("started = %v", got)
	}
}

func TestReleaseRepublish(t *testing.T) {
	env := setupRelease(t, "DATABASE_URL")
	releaseImage, releaseDigestFile, releaseCommit = env.image, "bundle.digest", "1eca7a8"
	if err := runReleasePublish(testCommand(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	raw, err := os.ReadFile("bundle.digest")
	if err != nil {
		t.Fatal(err)
	}
	original := strings.TrimSpace(string(raw))

	// Secrets are unavailable, and the secrets file is gone: it needs neither.
	if err := os.Remove(".azud/secrets"); err != nil {
		t.Fatal(err)
	}
	releaseDigestFile = "republished.digest"
	if err := runReleaseRepublish(testCommand(), []string{original}); err != nil {
		t.Fatalf("republish: %v", err)
	}
	raw, err = os.ReadFile("republished.digest")
	if err != nil {
		t.Fatal(err)
	}
	republished := strings.TrimSpace(string(raw))
	if republished == original {
		t.Fatal("the digest did not change")
	}
	if env.reg.Tag("acme/app", "production") != "" {
		t.Fatal("republish moved a tag")
	}

	before, err := release.Fetch(testCommand().Context(), env.repo, original)
	if err != nil {
		t.Fatal(err)
	}
	after, err := release.Fetch(testCommand().Context(), env.repo, republished)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Secrets, after.Secrets) || !after.Info.Created.After(before.Info.Created) {
		t.Fatalf("before %+v, after %+v", before.Info, after.Info)
	}

	if err := runReleaseRepublish(testCommand(), []string{"not-a-digest"}); err == nil {
		t.Fatal("republish took a tag")
	}
}
