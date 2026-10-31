package release

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/testutil"
)

const imageManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`

type fixture struct {
	reg   *testutil.Registry
	repo  *registry.Repository
	image Descriptor
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	reg := testutil.NewRegistry(t, "ci", "token")
	digest := reg.PutManifest("acme/app", "", manifestMediaType, []byte(imageManifest))
	resolver := &registry.Resolver{Client: reg.Client()}
	return fixture{
		reg:   reg,
		repo:  resolver.Repository(reg.Host(), "acme/app", &registry.Credentials{Username: "ci", Password: "token"}),
		image: Descriptor{MediaType: manifestMediaType, Digest: digest, Size: int64(len(imageManifest))},
	}
}

func (f fixture) info() Info {
	return Info{
		Service:     "app",
		Repository:  f.repo.Name(),
		Image:       f.image.Digest,
		Destination: "production",
		Commit:      "1eca7a80210c6d23a261a756ca4b5ce520240c0e",
		Created:     time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC),
		Secrets:     []string{"DATABASE_URL", "SECRET_KEY_BASE"},
		Recipients:  []string{"age1example"},
	}
}

var files = []File{
	{Name: "deploy.yml", Data: []byte("service: app\nimage: ghcr.io/acme/app\n")},
	{Name: "deploy.production.yml", Data: []byte("env:\n  clear:\n    MODE: production\n")},
}

func TestPublishedBundleReadsBackUntagged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	b, err := Build(f.info(), f.image, files, []byte("ciphertext"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Publish(ctx, f.repo, b); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if f.reg.Tag("acme/app", "production") != "" {
		t.Fatal("Publish tagged the bundle; only Promote may, after signing")
	}

	rel, err := Fetch(ctx, f.repo, b.Digest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if rel.Info.Commit != f.info().Commit || rel.Info.Image != f.image.Digest || strings.Join(rel.Info.Secrets, ",") != "DATABASE_URL,SECRET_KEY_BASE" {
		t.Fatalf("info = %+v", rel.Info)
	}
	if len(rel.Files) != 2 || rel.Files[1].Name != "deploy.production.yml" || string(rel.Files[0].Data) != string(files[0].Data) {
		t.Fatalf("files = %+v", rel.Files)
	}
	if string(rel.Secrets) != "ciphertext" {
		t.Fatalf("secrets = %q", rel.Secrets)
	}

	var m manifest
	if err := json.Unmarshal(b.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Subject == nil || m.Subject.Digest != f.image.Digest || m.ArtifactType != ArtifactType {
		t.Fatalf("manifest subject/artifactType = %+v %q", m.Subject, m.ArtifactType)
	}
}

func TestPromoteTagsOnlyAReleaseOfTheService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	info := f.info()
	info.Secrets = nil
	b, err := Build(info, f.image, files[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, f.repo, b); err != nil {
		t.Fatal(err)
	}

	if err := Promote(ctx, f.repo, b.Digest, "production", "other"); err == nil || !strings.Contains(err.Error(), `not "other"`) {
		t.Fatalf("Promote for another service = %v", err)
	}
	if err := Promote(ctx, f.repo, f.image.Digest, "production", "app"); err == nil || !strings.Contains(err.Error(), "not an azud release bundle") {
		t.Fatalf("Promote of an image = %v", err)
	}
	if f.reg.Tag("acme/app", "production") != "" {
		t.Fatal("a refused promotion moved the tag")
	}

	if err := Promote(ctx, f.repo, b.Digest, "production", "app"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if f.reg.Tag("acme/app", "production") != b.Digest {
		t.Fatal("production does not point to the bundle")
	}
}

func TestFetchRejectsABundleFromAnotherRepository(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	info.Repository = "ghcr.io/other/app"
	b, err := Build(info, f.image, files[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), f.repo, b.Digest); err == nil || !strings.Contains(err.Error(), "belongs to ghcr.io/other/app") {
		t.Fatalf("Fetch = %v, want a repository mismatch", err)
	}
}

func TestFetchRejectsTamperedContent(t *testing.T) {
	f := newFixture(t)
	b, err := Build(f.info(), f.image, files, []byte("ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	f.reg.SetBlob(registry.Digest(files[1].Data), []byte("env:\n  clear:\n    MODE: evil\n"))

	if _, err := Fetch(context.Background(), f.repo, b.Digest); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("Fetch = %v, want the tampered layer refused", err)
	}
}

func TestFetchRejectsUnexpectedLayers(t *testing.T) {
	f := newFixture(t)
	b, err := Build(f.info(), f.image, files[:1], []byte("ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b.Manifest, &m); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*manifest){
		"hook script":       func(m *manifest) { m.Layers[0].Annotations[titleAnnotation] = "pre-deploy" },
		"unknown type":      func(m *manifest) { m.Layers[0].MediaType = "application/x-sh" },
		"secrets elsewhere": func(m *manifest) { m.Layers[1].Annotations[titleAnnotation] = "deploy.age" },
		"not a release":     func(m *manifest) { m.ArtifactType = "application/vnd.example" },
	} {
		t.Run(name, func(t *testing.T) {
			var copied manifest
			_ = json.Unmarshal(b.Manifest, &copied)
			mutate(&copied)
			body, _ := json.Marshal(copied)
			digest := f.reg.PutManifest("acme/app", "", manifestMediaType, body)
			if _, err := Fetch(context.Background(), f.repo, digest); err == nil {
				t.Fatal("Fetch accepted the bundle")
			}
		})
	}
}

func TestFetchRequiresTheSecretsItLists(t *testing.T) {
	f := newFixture(t)
	b, err := Build(f.info(), f.image, files[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), f.repo, b.Digest); err == nil || !strings.Contains(err.Error(), "no secrets layer") {
		t.Fatalf("Fetch = %v, want the missing secrets layer refused", err)
	}
}

func TestBuildChecksItsInput(t *testing.T) {
	f := newFixture(t)
	cases := map[string]func() error{
		"image mismatch": func() error {
			other := f.image
			other.Digest = registry.Digest([]byte("x"))
			_, err := Build(f.info(), other, files, nil)
			return err
		},
		"deploy.yml first": func() error { _, err := Build(f.info(), f.image, files[1:], nil); return err },
		"path in a name": func() error {
			_, err := Build(f.info(), f.image, []File{{Name: "deploy.yml"}, {Name: "../deploy.x.yml"}}, nil)
			return err
		},
		"unsorted secrets": func() error {
			info := f.info()
			info.Secrets = []string{"Z", "A"}
			_, err := Build(info, f.image, files, nil)
			return err
		},
	}
	for name, build := range cases {
		if err := build(); err == nil {
			t.Errorf("%s: Build accepted it", name)
		}
	}
}

func TestDestinationNamesItsFile(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	info.Destination = "../../etc"
	if _, err := Build(info, f.image, files[:1], nil); err == nil || !strings.Contains(err.Error(), "invalid destination") {
		t.Fatalf("Build with a path as destination = %v", err)
	}
	info = f.info()
	info.Destination = "staging"
	if _, err := Build(info, f.image, files, nil); err == nil || !strings.Contains(err.Error(), "deploy.staging.yml") {
		t.Fatalf("Build with another destination's file = %v", err)
	}
}

func TestDottedDestinationPublishesAndReads(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	info.Destination = "eu.production"
	info.Secrets = nil
	b, err := Build(info, f.image, []File{files[0], {Name: "deploy.eu.production.yml", Data: files[1].Data}}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	rel, err := Fetch(context.Background(), f.repo, b.Digest)
	if err != nil || len(rel.Files) != 2 || rel.Files[1].Name != "deploy.eu.production.yml" {
		t.Fatalf("Fetch = %+v, %v", rel, err)
	}
}

func TestBuildRefusesOversizedReleaseJSON(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	for i := 0; i < 2000; i++ {
		info.Recipients = append(info.Recipients, "age1"+strings.Repeat("q", 58))
	}
	if _, err := Build(info, f.image, files, nil); err == nil || !strings.Contains(err.Error(), "release.json would be") {
		t.Fatalf("Build = %v, want an oversized release.json refused", err)
	}
}

func TestFetchRequiresTheDeclaredConfigSize(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	info.Secrets = nil
	b, err := Build(info, f.image, files[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	var m manifest
	_ = json.Unmarshal(b.Manifest, &m)
	m.Config.Size += 10
	body, _ := json.Marshal(m)
	digest := f.reg.PutManifest("acme/app", "", manifestMediaType, body)
	if _, err := Fetch(context.Background(), f.repo, digest); err == nil || !strings.Contains(err.Error(), "not the declared") {
		t.Fatalf("Fetch = %v, want a config size mismatch refused", err)
	}
}

func TestPromoteRefusesABundleServersCannotRead(t *testing.T) {
	f := newFixture(t)
	info := f.info()
	info.Repository = "ghcr.io/other/app"
	info.Secrets = nil
	b, err := Build(info, f.image, files[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(context.Background(), f.repo, b); err != nil {
		t.Fatal(err)
	}
	if err := Promote(context.Background(), f.repo, b.Digest, "production", "app"); err == nil {
		t.Fatal("Promote tagged a bundle from another repository")
	}
	if f.reg.Tag("acme/app", "production") != "" {
		t.Fatal("the tag moved")
	}
}
