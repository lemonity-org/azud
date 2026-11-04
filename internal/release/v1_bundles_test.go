package release

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/testutil"
)

// redirectHost sends requests for one registry host to the test registry, because a
// fixture names the repository it was published to and Fetch checks that.
type redirectHost struct {
	from, to string
	next     http.RoundTripper
}

func (r redirectHost) RoundTrip(req *http.Request) (*http.Response, error) {
	redirected := req
	if req.URL.Host == r.from {
		redirected = req.Clone(req.Context())
		redirected.URL.Host = r.to
		redirected.Host = r.to
	}
	resp, err := r.next.RoundTrip(redirected)
	if err != nil {
		return nil, err
	}
	// The client must keep seeing the host it asked for: it resolves an upload's
	// location against the request, and sends credentials only to that host.
	resp.Request = req
	if challenge := resp.Header.Get("Www-Authenticate"); challenge != "" {
		resp.Header.Set("Www-Authenticate", strings.ReplaceAll(challenge, r.to, r.from))
	}
	return resp, nil
}

// publishV1Bundle puts a fixture in a registry exactly as v1.6.0 wrote it, and reads
// it back with today's code.
func publishV1Bundle(t *testing.T, name string) *Release {
	t.Helper()
	read := func(file string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", "v1.6.0", name, file))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	reg := testutil.NewRegistry(t, "ci", "token")
	reg.PutManifest("acme/fixture", "", manifestMediaType, []byte(imageManifest))
	client := reg.Client()
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = redirectHost{from: "ghcr.io", to: reg.Host(), next: next}
	resolver := &registry.Resolver{Client: client}
	repo := resolver.Repository("ghcr.io", "acme/fixture", &registry.Credentials{Username: "ci", Password: "token"})

	digest := strings.TrimSpace(string(read("digest")))
	manifestJSON := read("manifest.json")
	if got := registry.Digest(manifestJSON); got != digest {
		t.Fatalf("manifest digest = %s, want %s", got, digest)
	}
	parsed, err := parseManifest(registry.Manifest{MediaType: manifestMediaType, Body: manifestJSON})
	if err != nil {
		t.Fatal(err)
	}
	blobs := [][]byte{read("release.json")}
	for _, layer := range parsed.Layers {
		blobs = append(blobs, read(layer.Annotations[titleAnnotation]))
	}
	ctx := context.Background()
	if err := Publish(ctx, repo, &Bundle{Digest: digest, Manifest: manifestJSON, blobs: blobs}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	rel, err := Fetch(ctx, repo, digest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return rel
}

func TestV1BundlesStayReadable(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		rel := publishV1Bundle(t, "minimal")
		if rel.Info.Format != 1 || rel.Info.Service != "fixture" || len(rel.Files) != 1 || rel.Secrets != nil {
			t.Fatalf("release = %+v", rel)
		}
		translated, _, notes, err := config.TranslateV1(rel.Files[0].Data, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(notes) != 0 {
			t.Errorf("notes = %q", notes)
		}
		if got := string(translated); !strings.Contains(got, "roles:") || strings.Contains(got, "hosts") {
			t.Errorf("translation =\n%s", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		rel := publishV1Bundle(t, "full")
		if rel.Info.Format != 1 || rel.Info.Destination != "production" || len(rel.Files) != 2 {
			t.Fatalf("release = %+v", rel)
		}
		base, dest, notes, err := config.TranslateV1(rel.Files[0].Data, rel.Files[1].Data)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"roles:", "${PUBLIC_HOST}", "cache:", "sweep:"} {
			if !strings.Contains(string(base), want) {
				t.Errorf("translation lacks %q:\n%s", want, base)
			}
		}
		for _, unwanted := range []string{"hosts", "db:", "ssh", "podman", "rollback_on_failure", "canary", "agent", "acme_email"} {
			if strings.Contains(string(base), unwanted) {
				t.Errorf("translation still has %q:\n%s", unwanted, base)
			}
		}
		if !strings.Contains(string(dest), "www.fixture.example.com") || !strings.Contains(string(dest), "roles:") {
			t.Errorf("destination translation =\n%s", dest)
		}
		if !slices.ContainsFunc(notes, func(note string) bool { return strings.HasPrefix(note, "accessories.db runs on another host") }) {
			t.Errorf("notes lack the accessory left out: %q", notes)
		}

		identity, err := os.ReadFile(filepath.Join("testdata", "v1.6.0", "full", "identity.txt"))
		if err != nil {
			t.Fatal(err)
		}
		secrets, err := Decrypt(rel.Secrets, identity)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if string(secrets) != "DATABASE_URL=postgres://fixture\n" {
			t.Errorf("secrets = %q", secrets)
		}
	})
}
