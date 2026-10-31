package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
	"github.com/lemonity-org/azud/internal/testutil"
)

type bundleEnv struct {
	t         *testing.T
	reg       *testutil.Registry
	repo      *registry.Repository
	image     string
	recipient string
	applier   *bundleApplier
	deployed  string
	deploys   []deployCall
	failNext  error
	envPath   string
	locked    bool
	accessory map[string]string
	fails     []error
	stopped   []string
	pending   []bool // Interrupted() as each deploy started
}

type deployCall struct {
	cfg    *config.Config
	image  string
	env    string // the secrets file when the deploy ran
	locked bool
}

func newBundleEnv(t *testing.T) *bundleEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	reg := testutil.NewRegistry(t, "reader", "token")
	image := reg.PutManifest("acme/app", "", "application/vnd.oci.image.manifest.v1+json", []byte(releaseTestImage))
	identityFile, recipient, err := release.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(home, ".config", "azud", "agent", "app.age")
	if err := os.MkdirAll(filepath.Dir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, identityFile, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	serverPath := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(serverPath, []byte(`service: app
image: `+reg.Host()+`/acme/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
registry:
  server: `+reg.Host()+`
  username: reader
  password: [AGENT_TEST_TOKEN]
agent:
  source: bundle
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := config.NewLoader(serverPath, "").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("server config: %v", err)
	}

	resolver := &registry.Resolver{Client: reg.Client()}
	e := &bundleEnv{
		t:         t,
		reg:       reg,
		repo:      resolver.Repository(reg.Host(), "acme/app", &registry.Credentials{Username: "reader", Password: "token"}),
		image:     image,
		recipient: recipient,
		envPath:   filepath.Join(home, ".azud", "secrets"),
	}
	e.applier = &bundleApplier{
		server:   server,
		repo:     e.repo,
		dir:      filepath.Join(home, "state", "releases"),
		identity: identity,
		deployed: func() (string, error) { return e.deployed, nil },
		withLock: func(fn func() error) error {
			e.locked = true
			defer func() { e.locked = false }()
			return fn()
		},
		deploy: func(_ context.Context, cfg *config.Config, image string) error {
			env, _ := os.ReadFile(e.envPath)
			e.deploys = append(e.deploys, deployCall{cfg: cfg, image: image, env: string(env), locked: e.locked})
			e.pending = append(e.pending, e.applier.Interrupted())
			if err := e.failNext; err != nil {
				e.failNext = nil
				return err
			}
			if len(e.fails) > 0 {
				err := e.fails[0]
				e.fails = e.fails[1:]
				return err
			}
			return nil
		},
		stopRoles: func(_ *config.Config, roles []string) error {
			e.stopped = append(e.stopped, roles...)
			return nil
		},
		version: "1.4.0",
		accessoryEnv: func(name string) (map[string]string, error) {
			if name != "db" {
				return nil, nil
			}
			return e.accessory, nil
		},
		log: output.DefaultLogger,
	}
	return e
}

// publish builds and pushes a bundle and returns its digest.
func (e *bundleEnv) publish(deployYAML, secrets string, created time.Time) string {
	e.t.Helper()
	full := "service: app\nimage: " + e.reg.Host() + "/acme/app\nproxy:\n  host: app.example.com\n" + deployYAML
	var names []string
	for name := range parseSecrets([]byte(secrets)) {
		names = append(names, name)
	}
	slices.Sort(names)
	var encrypted []byte
	if secrets != "" {
		var err error
		if encrypted, err = release.Encrypt([]byte(secrets), []string{e.recipient}); err != nil {
			e.t.Fatal(err)
		}
	}
	info := release.Info{Service: "app", Repository: e.repo.Name(), Image: e.image, Commit: "c-" + created.Format("150405"), Created: created, Secrets: names}
	b, err := release.Build(info, release.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: e.image, Size: int64(len(releaseTestImage))},
		[]release.File{{Name: "deploy.yml", Data: []byte(full)}}, encrypted)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := release.Publish(context.Background(), e.repo, b); err != nil {
		e.t.Fatal(err)
	}
	return b.Digest
}

func (e *bundleEnv) apply(digest string) error {
	err := e.applier.Deploy(context.Background(), e.repo.Name()+"@"+digest)
	if err == nil {
		e.deployed = digest
	}
	return err
}

func (e *bundleEnv) env() string {
	e.t.Helper()
	data, err := os.ReadFile(e.envPath)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

const webOnApp1 = "servers:\n  web:\n    hosts: [app-1]\nenv:\n  clear:\n    MODE: v1\n  secret: [DATABASE_URL]\n"

var day = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func TestFirstBundleDeploysWithItsConfigAndSecrets(t *testing.T) {
	e := newBundleEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.envPath, []byte("DATABASE_URL=pushed-by-env-push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := e.publish(webOnApp1, "DATABASE_URL=from-the-bundle\n", day)

	if err := e.apply(digest); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(e.deploys) != 1 {
		t.Fatalf("deploys = %d", len(e.deploys))
	}
	call := e.deploys[0]
	if call.image != e.reg.Host()+"/acme/app@"+e.image {
		t.Fatalf("deployed image = %s, want the bundle's image by digest", call.image)
	}
	if call.cfg.Env.Clear["MODE"] != "v1" || call.cfg.Registry.Username != "reader" {
		t.Fatalf("deploy config: env=%v registry=%+v", call.cfg.Env.Clear, call.cfg.Registry)
	}
	if e.env() != "DATABASE_URL=from-the-bundle\n" {
		t.Fatalf("secrets file = %q", e.env())
	}
	if info, err := os.Stat(e.envPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file mode = %v, %v", info.Mode(), err)
	}
	if backup, _ := os.ReadFile(e.envPath + ".pre-bundle"); string(backup) != "DATABASE_URL=pushed-by-env-push\n" {
		t.Fatalf("pre-bundle backup = %q", backup)
	}
	stored, err := e.applier.load(digest)
	if err != nil || stored == nil || stored.Info.Commit == "" {
		t.Fatalf("stored release = %+v, %v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(e.applier.releaseDir(digest), "secrets.age")); err != nil {
		t.Fatalf("encrypted secrets not kept: %v", err)
	}
}

func TestFailedFirstBundleRestoresTheSecretsFile(t *testing.T) {
	e := newBundleEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.envPath), 0o700)
	_ = os.WriteFile(e.envPath, []byte("DATABASE_URL=before\n"), 0o600)
	e.failNext = errors.New("health check failed")

	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=new\n", day)); err == nil || !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("Deploy = %v", err)
	}
	if e.env() != "DATABASE_URL=before\n" {
		t.Fatalf("secrets file after a failed deploy = %q, want it restored", e.env())
	}
}

func TestFailedReleaseRestoresThePreviousReleasesSecrets(t *testing.T) {
	e := newBundleEnv(t)
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=release-1\n", day)); err != nil {
		t.Fatal(err)
	}
	e.failNext = errors.New("migration failed")
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=release-2\n", day.Add(time.Hour))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if e.env() != "DATABASE_URL=release-1\n" {
		t.Fatalf("secrets file = %q, want release 1's secrets back", e.env())
	}
}

func TestOlderBundleIsRefusedUnlessAllowed(t *testing.T) {
	e := newBundleEnv(t)
	old := e.publish(webOnApp1, "DATABASE_URL=old\n", day)
	current := e.publish(webOnApp1, "DATABASE_URL=current\n", day.Add(24*time.Hour))
	if err := e.apply(current); err != nil {
		t.Fatal(err)
	}

	err := e.apply(old)
	if err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("Deploy of an older bundle = %v, want a refusal naming --allow-downgrade", err)
	}
	if len(e.deploys) != 1 || e.env() != "DATABASE_URL=current\n" {
		t.Fatalf("a refused downgrade deployed or touched secrets: deploys=%d env=%q", len(e.deploys), e.env())
	}

	e.applier.allowDowngrade = true
	if err := e.apply(old); err != nil {
		t.Fatalf("allowed rollback: %v", err)
	}
	if e.env() != "DATABASE_URL=old\n" {
		t.Fatalf("secrets after rollback = %q, want the old release's", e.env())
	}
}

func TestBundleMissingASecretIsRefused(t *testing.T) {
	e := newBundleEnv(t)
	cfg := "servers:\n  web:\n    hosts: [app-1]\nenv:\n  secret: [DATABASE_URL, SECRET_KEY_BASE]\n"
	if err := e.apply(e.publish(cfg, "DATABASE_URL=x\n", day)); err == nil || !strings.Contains(err.Error(), "SECRET_KEY_BASE") {
		t.Fatalf("Deploy = %v, want the missing secret named", err)
	}
	if len(e.deploys) != 0 {
		t.Fatal("deployed a release without its secrets")
	}
	if _, err := os.Stat(e.envPath); !os.IsNotExist(err) {
		t.Fatal("wrote the secrets file for a refused release")
	}
}

func TestBundleForAnotherHostIsRefused(t *testing.T) {
	e := newBundleEnv(t)
	cfg := "servers:\n  web:\n    hosts: [app-2]\n"
	if err := e.apply(e.publish(cfg, "A=1\n", day)); err == nil || !strings.Contains(err.Error(), "not this server") {
		t.Fatalf("Deploy = %v", err)
	}
	if len(e.deploys) != 0 {
		t.Fatal("deployed a release for another host")
	}
}

func TestChangesTheAgentDoesNotApplyStayReported(t *testing.T) {
	e := newBundleEnv(t)
	withDB := webOnApp1 + "accessories:\n  db:\n    image: postgres:17.9\n    host: app-1\n    env:\n      secret: [POSTGRES_PASSWORD]\n"
	e.accessory = map[string]string{"POSTGRES_PASSWORD": "one"}
	notes := func(digest string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(e.applier.releaseDir(digest), "notes.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	first := e.publish(withDB, "DATABASE_URL=a\nPOSTGRES_PASSWORD=one\n", day)
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes(first), "accessory db differs from this server's configuration") {
		t.Fatalf("first release notes = %s; the server's configuration has no db", notes(first))
	}
	if strings.Contains(notes(first), "another value") {
		t.Fatalf("first release notes = %s; db runs with the release's secret", notes(first))
	}

	second := e.publish(withDB, "DATABASE_URL=a\nPOSTGRES_PASSWORD=two\n", day.Add(time.Hour))
	if err := e.apply(second); err != nil {
		t.Fatal(err)
	}
	third := e.publish(withDB, "DATABASE_URL=b\nPOSTGRES_PASSWORD=two\n", day.Add(2*time.Hour))
	if err := e.apply(third); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{second, third} {
		if !strings.Contains(notes(digest), "accessory db runs with another value of secret POSTGRES_PASSWORD") ||
			!strings.Contains(notes(digest), "accessory db differs") {
			t.Fatalf("notes = %s; outstanding changes must stay reported until they match", notes(digest))
		}
	}

	e.accessory["POSTGRES_PASSWORD"] = "two" // rebooted from a workstation
	fourth := e.publish(withDB, "DATABASE_URL=c\nPOSTGRES_PASSWORD=two\n", day.Add(3*time.Hour))
	if err := e.apply(fourth); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notes(fourth), "another value") {
		t.Fatalf("notes = %s; db now runs with the release's secret", notes(fourth))
	}
	if len(e.deploys) != 4 {
		t.Fatalf("deploys = %d; unapplied changes must not block the app's deploy", len(e.deploys))
	}
}

func TestSecretsSwapAndDeployHoldTheDeployLock(t *testing.T) {
	e := newBundleEnv(t)
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=locked\n", day)); err != nil {
		t.Fatal(err)
	}
	call := e.deploys[0]
	if !call.locked || call.env != "DATABASE_URL=locked\n" {
		t.Fatalf("deploy ran locked=%v with secrets %q; the swap and the deploy must both hold the lock", call.locked, call.env)
	}
	if call.cfg.Deploy.RollbackOnFailure {
		t.Fatal("the deployer's fleet rollback is on; it would restart old roles with this release's secrets")
	}
}

func TestFailedReleaseRollsBackToThePreviousRelease(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=release-1\n", day)
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	second := strings.Replace(webOnApp1, "MODE: v1", "MODE: v2", 1)
	e.failNext = errors.New("worker failed to start")
	if err := e.apply(e.publish(second, "DATABASE_URL=release-2\n", day.Add(time.Hour))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if len(e.deploys) != 3 {
		t.Fatalf("deploys = %d, want release 1, release 2, then release 1 again", len(e.deploys))
	}
	rollback := e.deploys[2]
	if rollback.cfg.Env.Clear["MODE"] != "v1" || rollback.env != "DATABASE_URL=release-1\n" || !rollback.locked {
		t.Fatalf("rollback deploy: MODE=%q secrets=%q locked=%v", rollback.cfg.Env.Clear["MODE"], rollback.env, rollback.locked)
	}
}

func TestReleaseWithoutSecretsEmptiesTheFile(t *testing.T) {
	e := newBundleEnv(t)
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=old\n", day)); err != nil {
		t.Fatal(err)
	}
	none := "servers:\n  web:\n    hosts: [app-1]\n"
	if err := e.apply(e.publish(none, "", day.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if e.env() != "" {
		t.Fatalf("secrets file = %q; a release without secrets leaves none behind", e.env())
	}

	e.failNext = errors.New("boom")
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=new\n", day.Add(2*time.Hour))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if e.env() != "" {
		t.Fatalf("secrets file = %q, want the previous release's empty file back", e.env())
	}
}

func TestFailedFirstReleaseRemovesASecretsFileThatDidNotExist(t *testing.T) {
	e := newBundleEnv(t)
	e.failNext = errors.New("boom")
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=new\n", day)); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if _, err := os.Stat(e.envPath); !os.IsNotExist(err) {
		t.Fatalf("secrets file after a failed first release = %v, want it gone as before", err)
	}

	// A retry after an interrupted first attempt still restores "no file".
	_ = os.WriteFile(e.envPath, []byte("DATABASE_URL=left-by-the-crash\n"), 0o600)
	e.failNext = errors.New("boom again")
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=new\n", day.Add(time.Minute))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if _, err := os.Stat(e.envPath); !os.IsNotExist(err) {
		t.Fatal("the retry kept the file an interrupted attempt wrote")
	}
}

func TestIdentityReadableByOthersIsRefused(t *testing.T) {
	e := newBundleEnv(t)
	if err := os.Chmod(e.applier.identity, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=x\n", day)); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("Deploy = %v", err)
	}
}

func TestAgentInitCreatesAPrivateIdentityOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = &config.Config{Service: "app"}

	if err := runAgentInit(testCommand(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	path := filepath.Join(home, ".config", "azud", "agent", "app.age")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity = %v, %v", info, err)
	}
	first, _ := os.ReadFile(path)
	recipient := recipientFromIdentityFile(first)
	if err := release.ParseRecipient(recipient); err != nil {
		t.Fatalf("recipient %q: %v", recipient, err)
	}

	if err := runAgentInit(testCommand(), nil); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatal("init replaced an existing identity")
	}
}

func TestLocalSecretsPathExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/deploy")
	for in, want := range map[string]string{
		"$HOME/.azud/secrets":   "/home/deploy/.azud/secrets",
		"${HOME}/.azud/secrets": "/home/deploy/.azud/secrets",
		"~/.azud/secrets":       "/home/deploy/.azud/secrets",
		"/srv/app/secrets":      "/srv/app/secrets",
	} {
		if got, err := localSecretsPath(in); err != nil || got != want {
			t.Errorf("localSecretsPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRollbackStopsRolesTheFailedReleaseAdded(t *testing.T) {
	e := newBundleEnv(t)
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=a\n", day)); err != nil {
		t.Fatal(err)
	}
	withWorker := strings.Replace(webOnApp1, "servers:\n  web:\n    hosts: [app-1]\n", "servers:\n  web:\n    hosts: [app-1]\n  worker:\n    hosts: [app-1]\n    cmd: bin/worker\n", 1)
	e.failNext = errors.New("web failed after the worker started")
	if err := e.apply(e.publish(withWorker, "DATABASE_URL=b\n", day.Add(time.Hour))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if strings.Join(e.stopped, ",") != "worker" {
		t.Fatalf("stopped roles = %v; the worker the failed release added must not keep running it", e.stopped)
	}
	if e.applier.Interrupted() {
		t.Fatal("a completed rollback left the deploy marked interrupted")
	}
}

func TestDeployIsMarkedInterruptedUntilItFinishes(t *testing.T) {
	e := newBundleEnv(t)
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=a\n", day)); err != nil {
		t.Fatal(err)
	}
	if !e.pending[0] {
		t.Fatal("the deploy ran without the interrupted marker; a crash would go unnoticed")
	}
	if e.applier.Interrupted() {
		t.Fatal("the marker survived a finished deploy")
	}

	e.fails = []error{errors.New("release 2 failed"), errors.New("rollback failed too")}
	if err := e.apply(e.publish(webOnApp1, "DATABASE_URL=b\n", day.Add(time.Hour))); err == nil {
		t.Fatal("Deploy succeeded")
	}
	if !e.applier.Interrupted() {
		t.Fatal("a failed rollback must leave the deploy marked interrupted, so the next check redeploys")
	}
}

func TestBundleRequiringANewerAzudIsRefused(t *testing.T) {
	e := newBundleEnv(t)
	cfg := webOnApp1 + "minimum_version: \"9.0.0\"\n"
	if err := e.apply(e.publish(cfg, "DATABASE_URL=a\n", day)); err == nil || !strings.Contains(err.Error(), "9.0.0") {
		t.Fatalf("Deploy = %v, want the minimum version refused", err)
	}
	if len(e.deploys) != 0 {
		t.Fatal("deployed a release this azud is too old for")
	}
	if _, err := os.Stat(e.envPath); !os.IsNotExist(err) {
		t.Fatal("touched the secrets file for a refused release")
	}
}
