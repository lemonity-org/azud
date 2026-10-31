package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
	"github.com/lemonity-org/azud/internal/testutil"
)

const releaseTestImage = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`

type releaseEnv struct {
	reg      *testutil.Registry
	repo     *registry.Repository
	image    string
	identity []byte
}

// setupRelease writes a project with config/deploy.yml, a production
// destination and a secrets file, points the CLI's globals at it, and starts a
// registry holding one image.
func setupRelease(t *testing.T, secretNames string) releaseEnv {
	t.Helper()
	reg := testutil.NewRegistry(t, "ci", "registry-token")
	image := reg.PutManifest("acme/app", "", "application/vnd.oci.image.manifest.v1+json", []byte(releaseTestImage))
	identity, recipient, err := release.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config/deploy.yml", `service: app
image: `+reg.Host()+`/acme/app
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
registry:
  server: `+reg.Host()+`
  username: ci
  password: [RELEASE_TEST_REGISTRY_TOKEN]
env:
  secret: [`+secretNames+`]
secrets_path: .azud/secrets
release:
  recipients: [`+recipient+`]
`)
	write("config/deploy.production.yml", "env:\n  clear:\n    MODE: production\n")
	write(".azud/secrets", "DATABASE_URL=postgres://app:pw@db/app\nRELEASE_TEST_REGISTRY_TOKEN=registry-token\n")
	t.Setenv("RELEASE_TEST_REGISTRY_TOKEN", "registry-token")

	loaded, err := config.NewLoader("config/deploy.yml", "production").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	savedCfg, savedPath, savedDest, savedResolver := cfg, configPath, destination, newRegistryResolver
	t.Cleanup(func() {
		cfg, configPath, destination, newRegistryResolver = savedCfg, savedPath, savedDest, savedResolver
		releaseImage, releaseDigestFile, releaseCommit, releaseTrack = "", "", "", ""
	})
	cfg, configPath, destination = loaded, "config/deploy.yml", "production"
	newRegistryResolver = func() *registry.Resolver { return &registry.Resolver{Client: reg.Client()} }

	resolver := &registry.Resolver{Client: reg.Client()}
	return releaseEnv{
		reg:      reg,
		repo:     resolver.Repository(reg.Host(), "acme/app", &registry.Credentials{Username: "ci", Password: "registry-token"}),
		image:    image,
		identity: identity,
	}
}

func testCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

func TestReleasePublishPromoteAndRead(t *testing.T) {
	env := setupRelease(t, "DATABASE_URL")
	releaseImage, releaseDigestFile, releaseCommit = env.image, "bundle.digest", "1eca7a8"

	if err := runReleasePublish(testCommand(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	raw, err := os.ReadFile("bundle.digest")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(string(raw))
	if env.reg.Tag("acme/app", "production") != "" {
		t.Fatal("publish tagged the bundle")
	}

	rel, err := release.Fetch(context.Background(), env.repo, digest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if rel.Info.Image != env.image || rel.Info.Commit != "1eca7a8" || rel.Info.Destination != "production" {
		t.Fatalf("info = %+v", rel.Info)
	}
	if strings.Join(rel.Info.Secrets, ",") != "DATABASE_URL" {
		t.Fatalf("secrets = %v; the registry password is infrastructure and must stay out", rel.Info.Secrets)
	}
	if len(rel.Files) != 2 || rel.Files[1].Name != "deploy.production.yml" || !strings.Contains(string(rel.Files[1].Data), "MODE: production") {
		t.Fatalf("files = %+v", rel.Files)
	}
	plaintext, err := release.Decrypt(rel.Secrets, env.identity)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !strings.Contains(string(plaintext), "DATABASE_URL=postgres://app:pw@db/app") || strings.Contains(string(plaintext), "registry-token") {
		t.Fatalf("decrypted secrets = %q", plaintext)
	}

	releaseTrack = "production"
	if err := runReleasePromote(testCommand(), []string{digest}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if env.reg.Tag("acme/app", "production") != digest {
		t.Fatal("production does not point to the bundle")
	}
	if err := runReleaseInspect(testCommand(), []string{"production"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
}

func TestReleasePublishRefusesMissingSecrets(t *testing.T) {
	env := setupRelease(t, "DATABASE_URL, SECRET_KEY_BASE")
	releaseImage = env.image

	err := runReleasePublish(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "SECRET_KEY_BASE") {
		t.Fatalf("publish = %v, want the missing secret named", err)
	}
	if n := len(env.reg.Requests); n == 0 {
		t.Fatal("expected the image to be checked first")
	}
	for _, r := range env.reg.Requests {
		if strings.HasPrefix(r, "PUT ") {
			t.Fatalf("publish uploaded %s despite the missing secret", r)
		}
	}
}

func TestReleasePublishChecksItsInput(t *testing.T) {
	env := setupRelease(t, "DATABASE_URL")

	releaseImage = "latest"
	if err := runReleasePublish(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("publish with a tag = %v, want a digest required", err)
	}

	releaseImage = registry.Digest([]byte("not pushed"))
	if err := runReleasePublish(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("publish of a missing image = %v", err)
	}

	releaseImage = env.image
	cfg.Release.Recipients = nil
	if err := runReleasePublish(testCommand(), nil); err == nil || !strings.Contains(err.Error(), "release.recipients") {
		t.Fatalf("publish without recipients = %v", err)
	}
}

func TestParseRepositoryForReleases(t *testing.T) {
	ref, err := registry.ParseRepository("localhost:5000/acme/app")
	if err != nil || ref.Registry != "localhost:5000" || ref.Repository != "acme/app" || ref.Tag != "" {
		t.Fatalf("ParseRepository = %+v, %v", ref, err)
	}
	if _, err := registry.ParseRepository("ghcr.io/acme/app:v1"); err == nil {
		t.Fatal("ParseRepository accepted a tag")
	}
}
