package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/release"
)

func runnableCommands(root *cobra.Command) []*cobra.Command {
	var runnable []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() {
			runnable = append(runnable, c)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return runnable
}

// A command that matches no row must be effective, so a new one gets the strictest
// scope by declaring nothing, and a typo in an annotation fails here.
func TestEveryCommandHasKnownScope(t *testing.T) {
	known := []string{scopeEffective, scopeServer, scopeRepo, scopeEither, scopeNone}
	want := []struct{ prefix, scope string }{
		{"azud agent ", scopeServer},
		{"azud preflight", scopeServer},
		{"azud release ", scopeRepo},
		{"azud build", scopeRepo},
		{"azud env ", scopeEither},
		{"azud config", scopeEither},
		{"azud hooks ", scopeEither},
		{"azud version", scopeNone},
		{"azud init", scopeNone},
		{"azud completion", scopeNone},
		{"azud help", scopeNone},
		{"azud app ", scopeEffective},
		{"azud accessory ", scopeEffective},
		{"azud cron ", scopeEffective},
		{"azud proxy ", scopeEffective},
		{"azud history ", scopeEffective},
		// Removed at the flip, and read the file as written until then.
		{"azud deploy", scopeServer},
		{"azud redeploy", scopeServer},
		{"azud rollback", scopeServer},
		{"azud setup", scopeServer},
		{"azud registry ", scopeServer},
		{"azud server ", scopeServer},
		{"azud ssh ", scopeServer},
		{"azud systemd ", scopeServer},
	}
	commands := runnableCommands(rootCmd)
	if len(commands) < 50 {
		t.Fatalf("walked only %d commands", len(commands))
	}
	for _, c := range commands {
		got := commandScope(c)
		if !slices.Contains(known, got) {
			t.Errorf("%s has unknown scope %q", c.CommandPath(), got)
		}
		wantScope := scopeEffective
		for _, row := range want {
			if strings.HasPrefix(c.CommandPath(), row.prefix) {
				wantScope = row.scope
				break
			}
		}
		if got != wantScope {
			t.Errorf("%s has scope %q, want %q", c.CommandPath(), got, wantScope)
		}
		if need := commandRelease(c); !slices.Contains([]string{releaseWhole, releaseRunning, releaseSpec, releaseNone}, need) {
			t.Errorf("%s needs unknown release %q", c.CommandPath(), need)
		}
	}
}

func TestGeneratedCommandsLoadNothing(t *testing.T) {
	root := &cobra.Command{Use: "azud"}
	completion := &cobra.Command{Use: "completion"}
	completion.AddCommand(&cobra.Command{Use: "bash"})
	root.AddCommand(completion, &cobra.Command{Use: "help"}, &cobra.Command{Use: cobra.ShellCompRequestCmd}, &cobra.Command{Use: cobra.ShellCompNoDescRequestCmd})
	for _, args := range [][]string{{"completion", "bash"}, {"help"}, {cobra.ShellCompRequestCmd}, {cobra.ShellCompNoDescRequestCmd}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatal(err)
		}
		if got := commandScope(cmd); got != scopeNone {
			t.Errorf("%v has scope %q, want none", args, got)
		}
	}
}

// The list is exhaustive: the commands that read secrets are all here, so a command
// that stops needing them, or one that starts, shows up.
func TestCommandsThatLoadSecrets(t *testing.T) {
	loads := []string{
		"azud accessory boot",
		"azud agent run",
		"azud build",
		"azud deploy",
		"azud env delete",
		"azud env edit",
		"azud env get",
		"azud env list",
		"azud env pull",
		"azud env push",
		"azud env set",
		"azud preflight",
		"azud proxy boot",
		"azud proxy reboot",
		"azud redeploy",
		"azud registry login",
		"azud registry logout",
		"azud release publish",
		"azud rollback",
		"azud server bootstrap",
		"azud server exec",
		"azud setup",
		"azud ssh trust",
		"azud systemd enable",
	}
	for _, c := range runnableCommands(rootCmd) {
		if scope := commandScope(c); scope == scopeNone {
			continue
		}
		want := slices.Contains(loads, c.CommandPath())
		if got := !commandSkipsSecrets(c); got != want {
			t.Errorf("%s loads secrets = %v, want %v", c.CommandPath(), got, want)
		}
	}
}

func TestProxyReconcileLoadsSecretsOnlyToRepair(t *testing.T) {
	cmd := &cobra.Command{Use: "reconcile", Annotations: proxyReconcileCmd.Annotations}
	cmd.Flags().Bool("repair", false, "")
	if !commandSkipsSecrets(cmd) || commandRelease(cmd) != releaseNone {
		t.Errorf("a check loads secrets = %v and needs release %q", !commandSkipsSecrets(cmd), commandRelease(cmd))
	}
	if err := cmd.Flags().Set("repair", "true"); err != nil {
		t.Fatal(err)
	}
	if commandSkipsSecrets(cmd) || commandRelease(cmd) != releaseWhole {
		t.Errorf("a repair loads secrets = %v and needs release %q", !commandSkipsSecrets(cmd), commandRelease(cmd))
	}
}

func TestAccessoryBootWithAReleaseDoesNotReadWhatIsRunning(t *testing.T) {
	cmd := &cobra.Command{Use: "boot", Annotations: accessoryBootCmd.Annotations}
	cmd.Flags().String("release", "", "")
	if got := commandScope(cmd); got != scopeEffective {
		t.Errorf("scope = %q, want effective", got)
	}
	if err := cmd.Flags().Set("release", "production"); err != nil {
		t.Fatal(err)
	}
	if got := commandScope(cmd); got != scopeServer {
		t.Errorf("scope with --release = %q, want server", got)
	}
	if commandSkipsSecrets(cmd) {
		t.Error("--release resolves the tag with the registry password, so it must load secrets")
	}
}

// effectiveServer is an environment for loadConfig: a config file, a state directory,
// and a canary secrets_command.
type effectiveServer struct {
	t      *testing.T
	dir    string
	state  string
	canary string
	store  *agent.Store
}

func newEffectiveServer(t *testing.T, agentSection string) *effectiveServer {
	t.Helper()
	dir := t.TempDir()
	s := &effectiveServer{t: t, dir: dir, state: filepath.Join(dir, "state"), canary: filepath.Join(dir, "CANARY")}
	s.store = agent.NewStore(s.state, "shop")
	t.Setenv("AZUD_STATE_DIR", s.state)
	t.Setenv("AZUD_AGENT_DIGEST", "")
	path := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(path, []byte(s.config(agentSection)), 0o644); err != nil {
		t.Fatal(err)
	}
	previousPath, previousDestination, previousLogger := configPath, destination, output.DefaultLogger
	configPath, destination = path, ""
	output.DefaultLogger = output.NewLogger(&bytes.Buffer{}, &bytes.Buffer{}, false)
	t.Cleanup(func() {
		configPath, destination = previousPath, previousDestination
		output.DefaultLogger = previousLogger
	})
	return s
}

func (s *effectiveServer) config(agentSection string) string {
	return fmt.Sprintf(`service: shop
image: registry.test/shop:configured
servers:
  web:
    hosts: [10.0.0.1]
proxy:
  host: shop.test
secrets_provider: command
secrets_command: "touch %s"
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
%s`, s.canary, agentSection)
}

// deployed stores an image release and records it as what is deployed.
func (s *effectiveServer) deployed(digest string, edit func(string) string) {
	s.t.Helper()
	files := s.config("agent:\n  track: production\n  insecure_skip_verify: true\n")
	if edit != nil {
		files = edit(files)
	}
	// A record holds its files as ReadConfigFiles returns them.
	resolved, err := config.ResolveEnvReferences([]byte(files))
	if err != nil {
		s.t.Fatal(err)
	}
	files = string(resolved)
	id := strings.TrimPrefix(digest, "sha256:") + "-abcd1234"
	rec := &agent.Record{ID: id, Source: agent.SourceImage, Origin: agent.OriginVerified, Digest: digest, Image: "registry.test/shop@" + digest, Files: []release.File{{Name: "deploy.yml", Data: []byte(files)}}}
	if err := s.store.Records("registry.test/shop").Store(rec); err != nil {
		s.t.Fatal(err)
	}
	st, err := s.store.Load()
	if err != nil {
		s.t.Fatal(err)
	}
	st.DeployedDigest, st.Current = digest, id
	if err := s.store.Save(st); err != nil {
		s.t.Fatal(err)
	}
}

func (s *effectiveServer) settling(digest string) {
	s.t.Helper()
	st, err := s.store.Load()
	if err != nil {
		s.t.Fatal(err)
	}
	st.Journal = &agent.Journal{Target: strings.TrimPrefix(digest, "sha256:"), Digest: digest, Phase: agent.PhaseForward}
	if err := s.store.Save(st); err != nil {
		s.t.Fatal(err)
	}
}

func (s *effectiveServer) canaryRan() bool {
	_, err := os.Stat(s.canary)
	return err == nil
}

const (
	imageAgent = "agent:\n  track: production\n  insecure_skip_verify: true\n"
	digestOne  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestTwo  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestReadOnlyCommandsDoNotRunSecretsCommand(t *testing.T) {
	for name, section := range map[string]string{"no agent section": "", "agent section": imageAgent} {
		t.Run(name, func(t *testing.T) {
			s := newEffectiveServer(t, section)
			for _, c := range []*cobra.Command{
				appLogsCmd, appExecCmd, appDetailsCmd, accessoryLogsCmd, accessoryExecCmd,
				cronListCmd, cronLogsCmd, proxyLogsCmd, proxyStatusCmd, historyListCmd, historyShowCmd,
				hooksListCmd, configCmd, agentStatusCmd,
			} {
				if _, err := loadConfig(c); err != nil {
					t.Fatalf("%s: %v", c.CommandPath(), err)
				}
				if s.canaryRan() {
					t.Fatalf("%s ran secrets_command", c.CommandPath())
				}
			}
			// The canary works: a command that reads secrets runs it.
			if _, err := loadConfig(envGetCmd); err != nil {
				t.Fatal(err)
			}
			if !s.canaryRan() {
				t.Fatal("azud env get did not run secrets_command, so this proves little")
			}
		})
	}
}

func TestNoAgentSectionReadsTheFileAsWritten(t *testing.T) {
	s := newEffectiveServer(t, "")
	// Nothing reads this: a config with no agent section has no agent.
	s.deployed(digestOne, nil)
	s.settling(digestTwo)
	for _, c := range []*cobra.Command{cronBootCmd, cronRunCmd, accessoryBootCmd, appLogsCmd, historyListCmd, configCmd} {
		loaded, err := loadConfig(c)
		if err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		if loaded.Image != "registry.test/shop:configured" {
			t.Errorf("%s reads image %q, want the configured tag", c.CommandPath(), loaded.Image)
		}
	}
}

func TestEffectiveCommandsReadTheDeployedRelease(t *testing.T) {
	s := newEffectiveServer(t, imageAgent)
	s.deployed(digestOne, func(config string) string {
		return strings.Replace(config, "bin/backup", "bin/backup --from-the-release", 1)
	})
	for _, c := range []*cobra.Command{cronBootCmd, cronRunCmd, cronListCmd, proxyStatusCmd, historyListCmd} {
		loaded, err := loadConfig(c)
		if err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		if want := "registry.test/shop@" + digestOne; loaded.Image != want {
			t.Errorf("%s reads image %q, want %q", c.CommandPath(), loaded.Image, want)
		}
		if got := loaded.Cron["backup"].Command; got != "bin/backup --from-the-release" {
			t.Errorf("%s reads command %q, want the release's", c.CommandPath(), got)
		}
	}
	// Commands in other scopes read the file as written.
	for _, c := range []*cobra.Command{configCmd, agentStatusCmd, releaseInspectCmd, hooksListCmd} {
		loaded, err := loadConfig(c)
		if err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		if loaded.Image != "registry.test/shop:configured" {
			t.Errorf("%s reads image %q, want the configured tag", c.CommandPath(), loaded.Image)
		}
	}
}

func TestAnEditOnDiskIsNotAppliedAndCommandsThatWriteSaySo(t *testing.T) {
	s := newEffectiveServer(t, imageAgent)
	s.deployed(digestOne, nil)
	var stderr bytes.Buffer
	output.DefaultLogger = output.NewLogger(&bytes.Buffer{}, &stderr, false)
	if _, err := loadConfig(cronBootCmd); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("nothing was edited, and stderr = %q", stderr.String())
	}

	edited := strings.Replace(s.config(imageAgent), "bin/backup", "bin/edited", 1)
	if err := os.WriteFile(configPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(appLogsCmd); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a command that only reads warned: %q", stderr.String())
	}
	loaded, err := loadConfig(cronBootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Cron["backup"].Command; got != "bin/backup" {
		t.Errorf("command = %q, want the release's", got)
	}
	if !strings.Contains(stderr.String(), "configuration on disk changed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestBeforeTheFirstDeployCommandsThatNeedAReleaseSayWhatToDo(t *testing.T) {
	newEffectiveServer(t, imageAgent)
	for _, c := range []*cobra.Command{cronBootCmd, cronRunCmd} {
		_, err := loadConfig(c)
		if err == nil {
			t.Fatalf("%s ran with nothing deployed", c.CommandPath())
		}
		for _, want := range []string{"nothing is deployed", "production", "azud agent run --once", c.CommandPath()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", c.CommandPath(), err, want)
			}
		}
	}
}

func TestBeforeTheFirstDeployReadOnlyCommandsSayNothingIsDeployed(t *testing.T) {
	newEffectiveServer(t, imageAgent)
	var stderr bytes.Buffer
	output.DefaultLogger = output.NewLogger(&bytes.Buffer{}, &stderr, false)
	loaded, err := loadConfig(appLogsCmd)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Image != "registry.test/shop:configured" {
		t.Errorf("image = %q, want the file as written", loaded.Image)
	}
	if !strings.Contains(stderr.String(), "Nothing is deployed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestAccessoryBootBeforeTheFirstDeploy(t *testing.T) {
	t.Run("image mode keeps its spec on this server, so it needs no release", func(t *testing.T) {
		newEffectiveServer(t, imageAgent)
		if _, err := loadConfig(accessoryBootCmd); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a bundle carries the spec, so it names --release", func(t *testing.T) {
		newEffectiveServer(t, "agent:\n  track: production\n  source: bundle\n  verify_command: test -n \"$$AZUD_AGENT_IMAGE\"\n")
		_, err := loadConfig(accessoryBootCmd)
		if err == nil || !strings.Contains(err.Error(), "--release <tag>") {
			t.Fatalf("err = %v, want it to name --release", err)
		}
	})
}

func TestWhileSettling(t *testing.T) {
	s := newEffectiveServer(t, imageAgent)
	s.deployed(digestOne, nil)
	s.settling(digestTwo)

	for _, c := range []*cobra.Command{cronBootCmd, accessoryBootCmd} {
		_, err := loadConfig(c)
		if err == nil {
			t.Fatalf("%s ran while settling", c.CommandPath())
		}
		for _, want := range []string{digestTwo, c.CommandPath(), "azud agent run --once"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", c.CommandPath(), err, want)
			}
		}
	}

	repair := &cobra.Command{Use: "reconcile", Annotations: proxyReconcileCmd.Annotations}
	repair.Flags().Bool("repair", true, "")
	if _, err := loadConfig(repair); err == nil {
		t.Error("proxy reconcile --repair ran while settling")
	}

	for _, c := range []*cobra.Command{cronRunCmd, cronListCmd, appLogsCmd, appStopCmd, proxyStatusCmd, proxyBootCmd, historyListCmd, accessoryLogsCmd} {
		loaded, err := loadConfig(c)
		if err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		if want := "registry.test/shop@" + digestOne; loaded.Image != want {
			t.Errorf("%s reads image %q, want the last whole release %q", c.CommandPath(), loaded.Image, want)
		}
	}
}

// A bundle's deploy swaps the secrets file first, so a job started now would run the
// last whole release's image with the new release's secrets.
func TestCronRunRefusesWhileABundleDeploySettles(t *testing.T) {
	s := newEffectiveServer(t, "agent:\n  source: bundle\n  track: production\n  insecure_skip_verify: true\n")
	s.deployed(digestOne, nil)
	s.settling(digestTwo)

	_, err := loadConfig(cronRunCmd)
	if err == nil || !strings.Contains(err.Error(), digestTwo) {
		t.Fatalf("cron run while a bundle settles = %v", err)
	}
	if _, err := loadConfig(cronListCmd); err != nil {
		t.Errorf("cron list only reads, so it still runs: %v", err)
	}
}

func TestDeployCommandScriptActsForTheReleaseItIsDeploying(t *testing.T) {
	section := "agent:\n  track: production\n  insecure_skip_verify: true\n  deploy_command: exec deploy \"$$AZUD_AGENT_IMAGE\"\n"
	s := newEffectiveServer(t, section)
	s.deployed(digestOne, nil)
	s.settling(digestTwo)

	t.Setenv("AZUD_AGENT_DIGEST", digestTwo)
	repair := &cobra.Command{Use: "reconcile", Annotations: proxyReconcileCmd.Annotations}
	repair.Flags().Bool("repair", true, "")
	for _, c := range []*cobra.Command{repair, cronBootCmd} {
		loaded, err := loadConfig(c)
		if err != nil {
			t.Fatalf("%s from the release's own script: %v", c.CommandPath(), err)
		}
		if want := "registry.test/shop@" + digestTwo; loaded.Image != want {
			t.Errorf("%s reads image %q, want the release in progress %q", c.CommandPath(), loaded.Image, want)
		}
	}

	t.Setenv("AZUD_AGENT_DIGEST", digestOne)
	if _, err := loadConfig(cronBootCmd); err == nil {
		t.Error("a digest other than the journal's must not lift the refusal")
	}
}

func TestDeployCommandScriptBeforeTheFirstDeploy(t *testing.T) {
	s := newEffectiveServer(t, "agent:\n  track: production\n  insecure_skip_verify: true\n  deploy_command: exec deploy \"$$AZUD_AGENT_IMAGE\"\n")
	s.settling(digestOne)
	t.Setenv("AZUD_AGENT_DIGEST", digestOne)
	loaded, err := loadConfig(cronBootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if want := "registry.test/shop@" + digestOne; loaded.Image != want {
		t.Errorf("image = %q, want %q", loaded.Image, want)
	}
}

func TestStateThatCannotBeRead(t *testing.T) {
	s := newEffectiveServer(t, imageAgent)
	if err := os.MkdirAll(filepath.Dir(s.store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.store.Path(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	output.DefaultLogger = output.NewLogger(&bytes.Buffer{}, &stderr, false)

	// Logs must keep working in an incident.
	loaded, err := loadConfig(appLogsCmd)
	if err != nil {
		t.Fatalf("azud app logs: %v", err)
	}
	if loaded.Image != "registry.test/shop:configured" {
		t.Errorf("image = %q, want the file as written", loaded.Image)
	}
	if !strings.Contains(stderr.String(), "Cannot read what is running") {
		t.Errorf("stderr = %q", stderr.String())
	}

	// A command that writes from the release must not guess it.
	if _, err := loadConfig(cronBootCmd); err == nil || !strings.Contains(err.Error(), "cannot read what is running") {
		t.Errorf("azud cron boot: err = %v", err)
	}
}

func TestAccessoryReleaseNeedsABundleSource(t *testing.T) {
	var err error
	err = checkAccessoryReleaseSource(&config.Config{}, "db")
	if err == nil || !strings.Contains(err.Error(), "azud accessory boot db") {
		t.Errorf("no agent section: %v", err)
	}
	err = checkAccessoryReleaseSource(&config.Config{Agent: config.AgentConfig{Track: "production"}}, "db")
	if err == nil || !strings.Contains(err.Error(), "agent.source: bundle") || !strings.Contains(err.Error(), "azud accessory boot db") {
		t.Errorf("image mode: %v", err)
	}
	if err := checkAccessoryReleaseSource(&config.Config{Agent: config.AgentConfig{Source: config.AgentSourceBundle}}, "db"); err != nil {
		t.Errorf("bundle: %v", err)
	}
}

func TestAccessoryOnThisServer(t *testing.T) {
	bundle := config.AgentConfig{Source: config.AgentSourceBundle}
	release := &config.Config{
		Agent: bundle,
		Accessories: map[string]config.AccessoryConfig{
			"here":      {Host: "10.0.0.1"},
			"also":      {Hosts: []string{"10.0.0.2", "10.0.0.1"}},
			"elsewhere": {Host: "10.0.0.9"},
			"files":     {Host: "10.0.0.1", Files: []config.FileMapping{{Local: "/etc/shadow", Remote: "/srv/x"}}},
		},
	}
	for _, name := range []string{"here", "also"} {
		if _, err := accessoryOnThisServer(release, name, "10.0.0.1", "production"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, want := range map[string]string{
		"elsewhere": "10.0.0.9",
		"missing":   "has no accessory",
		"files":     "files is not applied from a release",
	} {
		_, err := accessoryOnThisServer(release, name, "10.0.0.1", "production")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}

func TestAccessoryFilesAreAppliedFromThisServersOwnConfiguration(t *testing.T) {
	accessory := config.AccessoryConfig{Files: []config.FileMapping{{Local: "a", Remote: "/b"}}}
	if err := checkAccessoryFiles(&config.Config{}, "db", accessory); err != nil {
		t.Errorf("no agent section: %v", err)
	}
	if err := checkAccessoryFiles(&config.Config{Agent: config.AgentConfig{Track: "production"}}, "db", accessory); err != nil {
		t.Errorf("image mode: %v", err)
	}
}

func TestWriteAccessorySecrets(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	runtimeFile := filepath.Join(t.TempDir(), ".azud", "secrets")
	path, remove, err := writeAccessorySecrets(runtimeFile, "db", []string{"DB_PASSWORD"}, map[string]string{"DB_PASSWORD": "s3cret", "APP_KEY": "other"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "DB_PASSWORD=s3cret\n" {
		t.Errorf("secrets file = %q, want only the accessory's", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}
	if filepath.Dir(path) != filepath.Dir(runtimeFile) || path == runtimeFile {
		t.Errorf("path = %s, want a file of its own beside %s", path, runtimeFile)
	}
	if _, err := os.Stat(runtimeFile); !os.IsNotExist(err) {
		t.Errorf("the runtime secrets file was created: %v", err)
	}
	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the file outlived the boot: %v", err)
	}

	if _, _, err := writeAccessorySecrets(runtimeFile, "db", []string{"MISSING"}, nil); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("a missing secret: %v", err)
	}
}
