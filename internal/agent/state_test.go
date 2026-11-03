package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/release"
)

// The digest and image of the fixture in internal/release/testdata/v1.6.0/full, a
// bundle v1.6.0 wrote.
const (
	v1FixtureDigest = "sha256:68d4796b65a7f4863fba80bc84bb00715c98d1cc4634feb4baad53f1cc88278e"
	v1FixtureImage  = "ghcr.io/acme/fixture@sha256:21d94c4ae81f56d81b98c4468796264cbff924c3fae1fd9e8379120a7d264f31"
	v1FailedDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	v1FixtureRecipient = "age1jj2dzlauutawpmt500nssnywk06suh7f5v0pqe026pc0cfqgketslj43y4"
)

// A state file as v1.6.0 wrote it, after the release in v1FailedDigest failed and
// left the server between releases.
const v160StateFile = `{
  "ref": "ghcr.io/acme/fixture:production",
  "deployed_digest": "` + v1FixtureDigest + `",
  "deployed_at": "2026-10-31T12:05:00.123456789Z",
  "failed_digest": "` + v1FailedDigest + `",
  "failed_at": "2026-10-31T13:00:00.5Z",
  "last_error": "deploy of ghcr.io/acme/fixture@` + v1FailedDigest + ` failed: health check failed",
  "last_seen_digest": "` + v1FailedDigest + `",
  "last_check_at": "2026-10-31T13:00:00.5Z"
}
`

// The server's configuration for the fixture, as it would be on disk.
const v1FixtureServer = `service: fixture
image: ghcr.io/acme/fixture
servers:
  web:
    hosts: [app-1]
proxy:
  host: fixture.example.com
ssh:
  user: deploy
agent:
  source: bundle
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
secrets_remote_path: /srv/fixture/secrets
`

// v1Dir lays out a state directory as v1.6.0 left it: the state file, a pending
// file if given, and the fixture stored as v1 stored it, with no record file.
func v1Dir(t *testing.T, stateFile, pending string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "agent", "fixture.json"), stateFile)
	releases := filepath.Join(dir, "agent", "fixture", "releases")
	if pending != "" {
		write(filepath.Join(releases, "pending"), pending)
	}
	stored := filepath.Join(releases, digestHex(v1FixtureDigest))
	for _, name := range []string{"release.json", "deploy.yml", "deploy.production.yml", "secrets.age"} {
		data, err := os.ReadFile(filepath.Join("..", "release", "testdata", "v1.6.0", "full", name))
		if err != nil {
			t.Fatal(err)
		}
		// The fixture leaves out the recipient, which validation needs. Its
		// release.json names the one it was encrypted to.
		write(filepath.Join(stored, name), strings.ReplaceAll(string(data), "RECIPIENT", v1FixtureRecipient))
	}
	write(filepath.Join(stored, "notes.json"), `{"ignored":["podman"],"not_applied":["cron jobs differ from this server's configuration (azud cron boot)"]}`)
	return dir
}

func TestV1StateAndPendingFileLoadAsTheyAre(t *testing.T) {
	dir := v1Dir(t, v160StateFile, v1FailedDigest+"\n")
	st, err := NewStore(dir, "fixture").Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.DeployedDigest != v1FixtureDigest || st.FailedDigest != v1FailedDigest || st.LastSeenDigest != v1FailedDigest {
		t.Fatalf("v1 fields = %+v", st)
	}
	if st.Current != digestHex(v1FixtureDigest) || len(st.Applied) != 1 || st.Applied[0] != st.Current {
		t.Fatalf("current = %q, applied = %v; both come from deployed_digest and the stored release", st.Current, st.Applied)
	}
	j := st.Journal
	if j == nil {
		t.Fatal("the pending file did not become a journal")
	}
	if j.Target != digestHex(v1FailedDigest) || j.Digest != v1FailedDigest || j.Phase != PhaseForward || j.First {
		t.Fatalf("journal = %+v", j)
	}
	if j.Previous != st.Current || j.PreviousDigest != v1FixtureDigest {
		t.Fatalf("journal previous = %q, %q", j.Previous, j.PreviousDigest)
	}
	if j.StartedAt.IsZero() {
		t.Fatal("the journal has no start time")
	}
}

func TestV1BackoffAndFirstReleaseCarryOver(t *testing.T) {
	after := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	stateFile := `{
  "ref": "ghcr.io/acme/fixture:production",
  "failed_digest": "` + v1FailedDigest + `",
  "recover_tries": 3,
  "recover_after": "` + after.Format(time.RFC3339) + `"
}
`
	dir := v1Dir(t, stateFile, v1FailedDigest+"\nfirst-release\n")
	store := NewStore(dir, "fixture")
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	j := st.Journal
	if j == nil || !j.First || j.Tries != 3 || !j.NextTry.Equal(after) || j.Previous != "" {
		t.Fatalf("journal = %+v", j)
	}
	if st.RecoverTries != 0 || !st.RecoverAfter.IsZero() {
		t.Fatalf("state = %+v; the backoff moved to the journal", st)
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "recover_") {
		t.Fatalf("saved state = %s; the journal is the only place for the backoff", data)
	}
}

func TestV1StateWithoutPendingFileHasNoJournal(t *testing.T) {
	stateFile := `{
  "ref": "r:t",
  "deployed_digest": "` + v1FixtureDigest + `",
  "recover_tries": 2,
  "recover_after": "2026-11-01T09:00:00Z"
}
`
	st, err := NewStore(v1Dir(t, stateFile, ""), "fixture").Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Journal != nil || st.RecoverTries != 0 || !st.RecoverAfter.IsZero() {
		t.Fatalf("state = %+v, journal %+v; nothing is interrupted", st, st.Journal)
	}
	if st.Current != digestHex(v1FixtureDigest) {
		t.Fatalf("current = %q", st.Current)
	}
}

func TestV1ImageStateHasNoCurrent(t *testing.T) {
	stateFile := `{
  "ref": "r:t",
  "deployed_digest": "` + digestA + `"
}
`
	st, err := NewStore(v1Dir(t, stateFile, ""), "fixture").Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.DeployedDigest != digestA || st.Current != "" || len(st.Applied) != 0 {
		t.Fatalf("state = %+v; v1 kept no record of an image", st)
	}
}

func TestStoredV1ReleaseWithNoRecordLoads(t *testing.T) {
	t.Setenv("PUBLIC_HOST", "fixture.example.com")
	dir := v1Dir(t, v160StateFile, "")
	rec, err := NewStore(dir, "fixture").Records("ghcr.io/acme/fixture").Load(digestHex(v1FixtureDigest))
	if err != nil || rec == nil {
		t.Fatalf("Load = %+v, %v", rec, err)
	}
	if rec.Source != SourceBundle || rec.Origin != OriginVerified || rec.Digest != v1FixtureDigest || rec.Image != v1FixtureImage {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Info == nil || rec.Info.Commit != "abc1234" || rec.Destination != "production" {
		t.Fatalf("release.json = %+v, destination %q", rec.Info, rec.Destination)
	}
	if len(rec.Files) != 2 || rec.Files[0].Name != "deploy.yml" || rec.Files[1].Name != "deploy.production.yml" {
		t.Fatalf("files = %+v", rec.Files)
	}
	if len(rec.Sealed) == 0 || len(rec.Ignored) != 1 || len(rec.NotApplied) != 1 {
		t.Fatalf("secrets %d bytes, notes %v %v", len(rec.Sealed), rec.Ignored, rec.NotApplied)
	}
	server := testServer(t, v1FixtureServer)
	cfg, _, err := rec.Config(server)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Proxy.PrimaryHost() != "www.fixture.example.com" {
		t.Fatalf("proxy host = %q; the destination file applies", cfg.Proxy.PrimaryHost())
	}
}

// v1State is the state file as v1.6.0 parses it.
type v1State struct {
	Ref            string    `json:"ref"`
	DeployedDigest string    `json:"deployed_digest,omitempty"`
	DeployedAt     time.Time `json:"deployed_at,omitzero"`
	FailedDigest   string    `json:"failed_digest,omitempty"`
	FailedAt       time.Time `json:"failed_at,omitzero"`
	LastError      string    `json:"last_error,omitempty"`
	LastSeenDigest string    `json:"last_seen_digest,omitempty"`
	LastCheckAt    time.Time `json:"last_check_at,omitzero"`
}

// The whole path: an upgraded server in the state v1.6.0 was stuck in undoes the
// failed release to a stored v1 release with no record, and leaves files v1 reads.
func TestV1StuckStateRecoversToAStoredV1Release(t *testing.T) {
	t.Setenv("PUBLIC_HOST", "fixture.example.com")
	dir := v1Dir(t, v160StateFile, v1FailedDigest+"\n")
	identity := filepath.Join(t.TempDir(), "identity.txt")
	data, err := os.ReadFile(filepath.Join("..", "release", "testdata", "v1.6.0", "full", "identity.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, data, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(t.TempDir(), "secrets")
	server := testServer(t, v1FixtureServer)

	type deployCall struct {
		image, secrets string
		undo, local    bool
	}
	var deploys []deployCall
	store := NewStore(dir, "fixture")
	a := &Agent{
		Ref:        "ghcr.io/acme/fixture:production",
		Repository: "ghcr.io/acme/fixture",
		Resolve:    func(context.Context) (string, error) { return v1FailedDigest, nil },
		Source:     &BundleSource{Server: server, Repository: "ghcr.io/acme/fixture", SecretsPath: secrets, Identity: identity, Log: output.DefaultLogger},
		Server:     server,
		Deploy: func(_ context.Context, _ *config.Config, image string, opts DeployOptions) error {
			env, _ := os.ReadFile(secrets)
			deploys = append(deploys, deployCall{image, string(env), opts.Undo, opts.Local})
			return nil
		},
		StopRoles:    func(*config.Config, []string) error { return nil },
		RunningRoles: func(*config.Config) ([]string, error) { return nil, nil },
		State:        store,
		Log:          output.DefaultLogger,
	}

	outcome, digest, err := a.Reconcile(context.Background(), false)
	if err != nil || outcome != Recovered || digest != v1FailedDigest {
		t.Fatalf("Reconcile = %v, %s, %v", outcome, digest, err)
	}
	if len(deploys) != 1 || deploys[0].image != v1FixtureImage || !deploys[0].undo || !deploys[0].local {
		t.Fatalf("deploys = %+v; want the stored release, as an undo", deploys)
	}
	if deploys[0].secrets != "DATABASE_URL=postgres://fixture\n" {
		t.Fatalf("secrets during the undo = %q; want the stored release's", deploys[0].secrets)
	}
	if _, err := os.Stat(store.pendingPath()); !os.IsNotExist(err) {
		t.Fatalf("pending file after the journal closed: %v", err)
	}

	// v1 ignores what it does not know, and finds its fields where it left them.
	data, err = os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var old v1State
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("v1 cannot parse the state: %v", err)
	}
	if old.DeployedDigest != v1FixtureDigest || old.FailedDigest != v1FailedDigest || old.Ref == "" {
		t.Fatalf("state as v1 reads it = %+v", old)
	}
	for _, name := range []string{"release.json", "deploy.yml", "deploy.production.yml", "secrets.age"} {
		if _, err := os.Stat(filepath.Join(a.records().Dir(digestHex(v1FixtureDigest)), name)); err != nil {
			t.Errorf("v1 reads %s of a stored release: %v", name, err)
		}
	}

	// The next check neither redeploys nor loops.
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != SkippedFailed || err != nil || len(deploys) != 1 {
		t.Fatalf("next check = %v, %v, deploys %d", outcome, err, len(deploys))
	}
}

func TestCurrentFollowsTheDeployedDigest(t *testing.T) {
	dir := v1Dir(t, v160StateFile, "")
	store := NewStore(dir, "fixture")
	// Another record's id, and a file whose deployed_digest changed since.
	st := State{DeployedDigest: v1FixtureDigest, Current: digestHex(v1FailedDigest), Applied: []string{digestHex(v1FailedDigest)}}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != digestHex(v1FixtureDigest) || got.Applied[0] != got.Current {
		t.Fatalf("current = %q, applied = %v", got.Current, got.Applied)
	}
}

func TestPendingFileWinsOnlyOverAJournalForAnotherRelease(t *testing.T) {
	dir := v1Dir(t, v160StateFile, v1FailedDigest+"\n")
	store := NewStore(dir, "fixture")
	same := &Journal{Target: digestHex(v1FailedDigest), Digest: v1FailedDigest, Phase: PhaseBack, Tries: 2}
	if err := store.Save(State{Journal: same}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Journal.Phase != PhaseBack || st.Journal.Tries != 2 {
		t.Fatalf("journal = %+v; an agent that died between saving the journal and removing the file keeps the journal", st.Journal)
	}

	other := &Journal{Target: digestHex(digestA), Digest: digestA, Phase: PhaseBack}
	if err := store.Save(State{Journal: other}); err != nil {
		t.Fatal(err)
	}
	if st, err = store.Load(); err != nil {
		t.Fatal(err)
	}
	if st.Journal.Digest != v1FailedDigest || st.Journal.Phase != PhaseForward {
		t.Fatalf("journal = %+v; a v1 binary wrote the pending file after this journal", st.Journal)
	}
}

func TestSaveRemovesThePendingFileOnlyOnceItIsAJournal(t *testing.T) {
	dir := v1Dir(t, v160StateFile, v1FailedDigest+"\n")
	store := NewStore(dir, "fixture")
	if err := store.Save(State{Ref: "r:t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.pendingPath()); err != nil {
		t.Fatalf("a state that never held the pending file removed it: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.pendingPath()); !os.IsNotExist(err) {
		t.Fatalf("pending file = %v; the journal replaces it", err)
	}
	if st, err = store.Load(); err != nil || st.Journal == nil || st.Journal.Digest != v1FailedDigest {
		t.Fatalf("journal = %+v, %v; it must survive the file", st.Journal, err)
	}
}

func TestRecordsAreImmutableAndAppearWhole(t *testing.T) {
	records := NewStore(t.TempDir(), "app").Records(testRepository)
	rec := &Record{
		ID:     "abc-12345678",
		Source: SourceImage,
		Origin: OriginVerified,
		Digest: "sha256:abc",
		Image:  testRepository + "@sha256:abc",
		Files:  []release.File{{Name: "deploy.yml", Data: []byte("one")}},
	}
	if err := records.Store(rec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(records.Dir(rec.ID), "deploy.yml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored file = %v, %v; want it private", info, err)
	}

	changed := *rec
	changed.Origin = OriginAdopted
	changed.Files = []release.File{{Name: "deploy.yml", Data: []byte("two")}}
	if err := records.Store(&changed); err != nil {
		t.Fatal(err)
	}
	got, err := records.Load(rec.ID)
	if err != nil || got.Origin != OriginVerified || string(got.Files[0].Data) != "one" {
		t.Fatalf("Load = %+v, %v; a record never changes", got, err)
	}

	entries, err := os.ReadDir(filepath.Dir(records.Dir(rec.ID)))
	if err != nil || len(entries) != 1 {
		t.Fatalf("releases directory = %v, %v; a store leaves nothing else behind", entries, err)
	}

	if missing, err := records.Load("nothing-here"); missing != nil || err != nil {
		t.Fatalf("Load of a missing record = %+v, %v", missing, err)
	}
}

func TestBrokenStoredReleaseIsReplaced(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	records := store.Records(testRepository)
	// What a v1 agent that died while storing leaves.
	dir := records.Dir("aaa")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"format":1,"image":"sha256:img"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec, err := records.Load("aaa"); err == nil {
		t.Fatalf("Load = %+v; a release with no deploy.yml cannot be deployed", rec)
	}
	rec := &Record{
		ID:     "aaa",
		Source: SourceBundle,
		Origin: OriginVerified,
		Digest: "sha256:aaa",
		Image:  testRepository + "@sha256:img",
		Info:   &release.Info{Image: "sha256:img"},
		Files:  []release.File{{Name: "deploy.yml", Data: []byte("whole")}},
	}
	if err := records.Store(rec); err != nil {
		t.Fatal(err)
	}
	got, err := records.Load("aaa")
	if err != nil || string(got.Files[0].Data) != "whole" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestImageRecordIDNamesTheDigestAndItsConfiguration(t *testing.T) {
	files := []release.File{{Name: "deploy.yml", Data: []byte("service: app\n")}, {Name: "deploy.agent.yml", Data: []byte("agent: {}\n")}}
	sum := sha256.Sum256([]byte("service: app\nagent: {}\n"))
	id := imageRecordID(digestA, files)
	if want := digestHex(digestA) + "-" + hex.EncodeToString(sum[:])[:8]; id != want {
		t.Fatalf("id = %q, want %q", id, want)
	}
	edited := []release.File{{Name: "deploy.yml", Data: []byte("service: app\n")}, {Name: "deploy.agent.yml", Data: []byte("agent: {track: x}\n")}}
	if imageRecordID(digestA, edited) == id {
		t.Fatal("an edited configuration kept the record id")
	}
	if recordDigest(id) != digestHex(digestA) {
		t.Fatalf("recordDigest = %q", recordDigest(id))
	}
}

func TestReadConfigFiles(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(base, []byte("service: base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := ReadConfigFiles(base, "")
	if err != nil || len(files) != 1 || files[0].Name != "deploy.yml" || string(files[0].Data) != "service: base\n" {
		t.Fatalf("files = %+v, %v", files, err)
	}
	// A destination with no file of its own is allowed, as it is for the loader.
	files, err = ReadConfigFiles(base, "agent")
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.agent.yaml"), []byte("service: dest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err = ReadConfigFiles(base, "agent")
	if err != nil || len(files) != 2 || files[1].Name != "deploy.agent.yml" || string(files[1].Data) != "service: dest\n" {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if _, err := ReadConfigFiles(filepath.Join(dir, "nothing"), ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing configuration = %v", err)
	}
}
