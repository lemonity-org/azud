package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/testutil"
)

const ociManifest = "application/vnd.oci.image.manifest.v1+json"

func testRepository(t *testing.T) (*testutil.Registry, *Repository) {
	t.Helper()
	reg := testutil.NewRegistry(t, "deploy", "s3cret")
	resolver := &Resolver{Client: reg.Client()}
	return reg, resolver.Repository(reg.Host(), "lemonity-org/app", &Credentials{Username: "deploy", Password: "s3cret"})
}

func countRequests(reg *testutil.Registry, prefix string) int {
	n := 0
	for _, r := range reg.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func TestPushBlobUploadsOnceAndReadsBack(t *testing.T) {
	reg, repo := testRepository(t)
	ctx := context.Background()

	digest, err := repo.PushBlob(ctx, []byte("release.json"))
	if err != nil {
		t.Fatalf("PushBlob: %v", err)
	}
	if digest != Digest([]byte("release.json")) {
		t.Fatalf("digest = %s", digest)
	}
	if _, err := repo.PushBlob(ctx, []byte("release.json")); err != nil {
		t.Fatalf("second PushBlob: %v", err)
	}
	if n := countRequests(reg, "PUT /v2/lemonity-org/app/blobs/uploads/"); n != 1 {
		t.Fatalf("uploads = %d, want 1: a blob the registry has is not uploaded again", n)
	}

	got, err := repo.Blob(ctx, digest, 1024)
	if err != nil || string(got) != "release.json" {
		t.Fatalf("Blob = %q, %v", got, err)
	}
}

func TestUploadAsksForPushScopeAfterPull(t *testing.T) {
	reg, repo := testRepository(t)
	ctx := context.Background()
	reg.PutManifest("lemonity-org/app", "v1", ociManifest, []byte(`{"schemaVersion":2}`))

	if _, err := repo.Manifest(ctx, "v1"); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if _, err := repo.PushBlob(ctx, []byte("data")); err != nil {
		t.Fatalf("PushBlob: %v", err)
	}
	want := []string{"repository:lemonity-org/app:pull", "repository:lemonity-org/app:pull,push"}
	if len(reg.Scopes) != 2 || reg.Scopes[0] != want[0] || reg.Scopes[1] != want[1] {
		t.Fatalf("token scopes = %v, want %v", reg.Scopes, want)
	}
}

func TestPutManifestByDigestAndTag(t *testing.T) {
	reg, repo := testRepository(t)
	ctx := context.Background()
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest := Digest(body)

	if got, err := repo.PutManifest(ctx, digest, ociManifest, body); err != nil || got != digest {
		t.Fatalf("PutManifest by digest = %s, %v", got, err)
	}
	if reg.Tag("lemonity-org/app", "production") != "" {
		t.Fatal("a manifest pushed by digest must not be tagged")
	}
	if _, err := repo.PutManifest(ctx, "production", ociManifest, body); err != nil {
		t.Fatalf("PutManifest by tag: %v", err)
	}
	if reg.Tag("lemonity-org/app", "production") != digest {
		t.Fatal("tag does not point to the manifest")
	}

	m, err := repo.Manifest(ctx, "production")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Digest != digest || m.MediaType != ociManifest || string(m.Body) != string(body) {
		t.Fatalf("Manifest = %+v", m)
	}
}

func TestPutManifestRefusesAMismatchedDigest(t *testing.T) {
	reg, repo := testRepository(t)
	_, err := repo.PutManifest(context.Background(), Digest([]byte("other")), ociManifest, []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("PutManifest = %v, want a digest mismatch", err)
	}
	if len(reg.Requests) != 0 {
		t.Fatalf("requests sent: %v", reg.Requests)
	}
}

func TestBlobRejectsContentThatDoesNotMatchItsDigest(t *testing.T) {
	reg, repo := testRepository(t)
	digest := Digest([]byte("signed config"))
	reg.SetBlob(digest, []byte("tampered config"))

	_, err := repo.Blob(context.Background(), digest, 1024)
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("Blob = %v, want a digest mismatch", err)
	}
}

func TestBlobRefusesOversizedContent(t *testing.T) {
	_, repo := testRepository(t)
	digest, err := repo.PushBlob(context.Background(), []byte(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Blob(context.Background(), digest, 99); err == nil || !strings.Contains(err.Error(), "exceeds 99 bytes") {
		t.Fatalf("Blob = %v, want a size error", err)
	}
}

func TestManifestByDigestIsChecked(t *testing.T) {
	reg, repo := testRepository(t)
	digest := reg.PutManifest("lemonity-org/app", "", ociManifest, []byte(`{"a":1}`))

	if _, err := repo.Manifest(context.Background(), digest); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if _, err := repo.Manifest(context.Background(), Digest([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest error = %v, want ErrNotFound", err)
	}
}

func TestRepositoryRejectsWrongCredentials(t *testing.T) {
	reg := testutil.NewRegistry(t, "deploy", "s3cret")
	resolver := &Resolver{Client: reg.Client()}
	repo := resolver.Repository(reg.Host(), "lemonity-org/app", &Credentials{Username: "deploy", Password: "wrong"})

	if _, err := repo.PushBlob(context.Background(), []byte("x")); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("PushBlob = %v, want the token service's refusal", err)
	}
}
