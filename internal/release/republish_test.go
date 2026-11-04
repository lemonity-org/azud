package release

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/registry"
)

func (f fixture) infoWithoutSecrets() Info {
	info := f.info()
	info.Secrets = nil
	return info
}

func TestRepublishCopiesTheReleaseWithANewCreationTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	secrets := []byte("ciphertext that cannot be decrypted here")
	original, err := Build(f.info(), f.image, files, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, f.repo, original); err != nil {
		t.Fatal(err)
	}

	later := f.info().Created.Add(30 * 24 * time.Hour)
	bundle, err := Republish(ctx, f.repo, original.Digest, "app", later)
	if err != nil {
		t.Fatalf("Republish: %v", err)
	}
	if bundle.Digest == original.Digest {
		t.Fatal("the digest did not change, so the old signature would still apply")
	}
	if f.reg.Tag("acme/app", "production") != "" {
		t.Fatal("Republish tagged the bundle; only Promote may, after signing")
	}

	got, err := Fetch(ctx, f.repo, bundle.Digest)
	if err != nil {
		t.Fatalf("Fetch of what was republished: %v", err)
	}
	if !got.Info.Created.Equal(later) {
		t.Errorf("created = %s, want %s", got.Info.Created, later)
	}
	if !bytes.Equal(got.Secrets, secrets) {
		t.Errorf("secrets changed: %q", got.Secrets)
	}
	if got.Info.Image != f.image.Digest || got.Subject.Digest != f.image.Digest || got.Subject.MediaType != f.image.MediaType || got.Subject.Size != f.image.Size {
		t.Errorf("image = %s, subject %+v", got.Info.Image, got.Subject)
	}
	want := f.info()
	if got.Info.Commit != want.Commit || got.Info.Destination != want.Destination || strings.Join(got.Info.Secrets, ",") != strings.Join(want.Secrets, ",") || strings.Join(got.Info.Recipients, ",") != strings.Join(want.Recipients, ",") {
		t.Errorf("info = %+v, want the original's", got.Info)
	}
	if len(got.Files) != len(files) {
		t.Fatalf("files = %d", len(got.Files))
	}
	for i, file := range got.Files {
		if file.Name != files[i].Name || !bytes.Equal(file.Data, files[i].Data) {
			t.Errorf("file %d = %s", i, file.Name)
		}
	}

	var m manifest
	if err := json.Unmarshal(bundle.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Annotations[RepublishedFromAnnotation] != original.Digest {
		t.Errorf("annotations = %v", m.Annotations)
	}
	if m.Annotations["org.opencontainers.image.created"] != later.Format(time.RFC3339) {
		t.Errorf("annotations = %v", m.Annotations)
	}
}

// parseInfo refuses a field it does not know, as v1's does, so provenance must not be in
// release.json.
func TestRepublishedReleaseJSONHasOnlyWhatAV1AgentReads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	original, err := Build(f.infoWithoutSecrets(), f.image, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, f.repo, original); err != nil {
		t.Fatal(err)
	}
	bundle, err := Republish(ctx, f.repo, original.Digest, "app", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle.blobs[0]), "republish") || strings.Contains(string(bundle.blobs[0]), original.Digest) {
		t.Errorf("release.json carries provenance: %s", bundle.blobs[0])
	}
	if _, err := parseInfo(bundle.blobs[0]); err != nil {
		t.Errorf("parseInfo: %v", err)
	}
	var rawInfo map[string]any
	if err := json.Unmarshal(bundle.blobs[0], &rawInfo); err != nil {
		t.Fatal(err)
	}
	if rawInfo["format"] != float64(1) {
		t.Errorf("format = %v, want the source's", rawInfo["format"])
	}
}

func TestRepublishRefusesWhatItCannotPublish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	original, err := Build(f.infoWithoutSecrets(), f.image, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, f.repo, original); err != nil {
		t.Fatal(err)
	}

	if _, err := Republish(ctx, f.repo, original.Digest, "another", time.Now()); err == nil || !strings.Contains(err.Error(), "is a release of") {
		t.Errorf("Republish of another service's release = %v", err)
	}
	if _, err := Republish(ctx, f.repo, f.image.Digest, "app", time.Now()); err == nil {
		t.Error("Republish of an image that is not a bundle succeeded")
	}
	if _, err := Republish(ctx, f.repo, "not-a-digest", "app", time.Now()); err == nil {
		t.Error("Republish of a malformed digest succeeded")
	}
	missing := registry.Digest([]byte("nothing here"))
	if _, err := Republish(ctx, f.repo, missing, "app", time.Now()); err == nil {
		t.Error("Republish of an unknown digest succeeded")
	}
}

func TestRepublishRefusesARemovedImage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	info := f.infoWithoutSecrets()
	info.Image = registry.Digest([]byte("an image that was deleted"))
	gone := Descriptor{MediaType: f.image.MediaType, Digest: info.Image, Size: 1}
	original, err := Build(info, gone, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, f.repo, original); err != nil {
		t.Fatal(err)
	}
	if _, err := Republish(ctx, f.repo, original.Digest, "app", time.Now()); err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("Republish = %v; a tag must not point at a release with no image", err)
	}
}

// A bundle v1.6.0 wrote republishes byte for byte, with no key that opens its secrets.
func TestRepublishOfAV1Bundle(t *testing.T) {
	rel := publishV1Bundle(t, "full")
	// The fixture's image is not in the test registry, so this builds what Republish builds
	// from a release it read, without the check that the image is still there.
	info := rel.Info
	info.Created = rel.Info.Created.Add(time.Hour)
	bundle, err := build(info, rel.Subject, rel.Files, rel.Secrets, map[string]string{RepublishedFromAnnotation: rel.Digest})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(bundle.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Subject == nil || m.Subject.Digest != rel.Info.Image {
		t.Errorf("subject = %+v", m.Subject)
	}
	var sawSecrets bool
	for _, blob := range bundle.blobs {
		sawSecrets = sawSecrets || bytes.Equal(blob, rel.Secrets)
	}
	if !sawSecrets {
		t.Error("the secrets layer changed")
	}
	if info, err := parseInfo(bundle.blobs[0]); err != nil || info.Format != 1 {
		t.Errorf("parseInfo = %+v, %v", info, err)
	}
}
