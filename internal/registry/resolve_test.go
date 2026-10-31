package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`

func manifestDigest() string {
	sum := sha256.Sum256([]byte(testManifest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeRegistry serves one repository the way ghcr.io and Forgejo do: manifest
// requests need a bearer token from /token, which needs basic credentials
// unless the registry is public.
type fakeRegistry struct {
	server        *httptest.Server
	public        bool
	omitDigest    bool
	rejectHead    bool
	tokenRequests int
	lastScope     string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenRequests++
		f.lastScope = r.URL.Query().Get("scope")
		user, pass, ok := r.BasicAuth()
		if !f.public && (!ok || user != "deploy" || pass != "s3cret") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"granted"}`)
	})
	mux.HandleFunc("/v2/lemonity-org/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer granted" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:lemonity-org/app:pull"`, f.server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			t.Errorf("manifest request Accept = %q, want OCI index accepted", r.Header.Get("Accept"))
		}
		if strings.TrimPrefix(r.URL.Path, "/v2/lemonity-org/app/manifests/") != "production" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodHead && f.rejectHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !f.omitDigest {
			w.Header().Set("Docker-Content-Digest", manifestDigest())
		}
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, testManifest)
		}
	})
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) resolver() *Resolver {
	return &Resolver{Client: f.server.Client()}
}

func (f *fakeRegistry) ref(tag string) Reference {
	return Reference{Registry: f.server.Listener.Addr().String(), Repository: "lemonity-org/app", Tag: tag}
}

func TestDigestUsesBearerTokenWithCredentials(t *testing.T) {
	f := newFakeRegistry(t)

	digest, err := f.resolver().Digest(context.Background(), f.ref("production"), &Credentials{Username: "deploy", Password: "s3cret"})
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if digest != manifestDigest() {
		t.Fatalf("digest = %s, want %s", digest, manifestDigest())
	}
	if f.lastScope != "repository:lemonity-org/app:pull" {
		t.Fatalf("token scope = %q, want pull on the repository", f.lastScope)
	}
}

func TestDigestRejectsWrongCredentials(t *testing.T) {
	f := newFakeRegistry(t)

	_, err := f.resolver().Digest(context.Background(), f.ref("production"), &Credentials{Username: "deploy", Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "token service returned HTTP 401") {
		t.Fatalf("Digest error = %v, want token rejection", err)
	}
}

func TestDigestOfPublicImageNeedsNoCredentials(t *testing.T) {
	f := newFakeRegistry(t)
	f.public = true

	digest, err := f.resolver().Digest(context.Background(), f.ref("production"), nil)
	if err != nil || digest != manifestDigest() {
		t.Fatalf("Digest = %s, %v", digest, err)
	}
}

func TestDigestHashesManifestWhenHeaderIsMissing(t *testing.T) {
	f := newFakeRegistry(t)
	f.public = true
	f.omitDigest = true

	digest, err := f.resolver().Digest(context.Background(), f.ref("production"), nil)
	if err != nil || digest != manifestDigest() {
		t.Fatalf("Digest = %s, %v; want the manifest hash", digest, err)
	}
}

func TestDigestFallsBackToGetWhenHeadIsRejected(t *testing.T) {
	f := newFakeRegistry(t)
	f.public = true
	f.rejectHead = true

	digest, err := f.resolver().Digest(context.Background(), f.ref("production"), nil)
	if err != nil || digest != manifestDigest() {
		t.Fatalf("Digest = %s, %v", digest, err)
	}
}

func TestDigestReportsMissingTag(t *testing.T) {
	f := newFakeRegistry(t)
	f.public = true

	_, err := f.resolver().Digest(context.Background(), f.ref("staging"), nil)
	if !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("Digest error = %v, want ErrTagNotFound", err)
	}
}

func TestDigestRejectsMismatchedHeader(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = fmt.Fprint(w, testManifest)
	}))
	defer server.Close()

	resolver := &Resolver{Client: server.Client()}
	ref := Reference{Registry: server.Listener.Addr().String(), Repository: "lemonity-org/app", Tag: "production"}
	if _, err := resolver.Digest(context.Background(), ref, nil); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("Digest error = %v, want digest mismatch", err)
	}
}

func TestDigestRefusesPlainHTTPTokenService(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://attacker.test/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	resolver := &Resolver{Client: server.Client()}
	ref := Reference{Registry: server.Listener.Addr().String(), Repository: "lemonity-org/app", Tag: "production"}
	_, err := resolver.Digest(context.Background(), ref, &Credentials{Username: "deploy", Password: "s3cret"})
	if err == nil || !strings.Contains(err.Error(), "not HTTPS") {
		t.Fatalf("Digest error = %v, want refusal to send credentials over HTTP", err)
	}
}

func TestDigestRejectsMalformedDigestHeader(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:not-a-digest")
	}))
	defer server.Close()

	resolver := &Resolver{Client: server.Client()}
	ref := Reference{Registry: server.Listener.Addr().String(), Repository: "lemonity-org/app", Tag: "production"}
	if _, err := resolver.Digest(context.Background(), ref, nil); err == nil || !strings.Contains(err.Error(), "malformed digest") {
		t.Fatalf("Digest error = %v, want malformed digest", err)
	}
}

func TestParseReference(t *testing.T) {
	tests := []struct {
		in      string
		want    Reference
		wantErr bool
	}{
		{in: "ghcr.io/lemonity-org/app:production", want: Reference{"ghcr.io", "lemonity-org/app", "production"}},
		{in: "localhost:5000/app:v1", want: Reference{"localhost:5000", "app", "v1"}},
		{in: "nginx:1.27", want: Reference{"docker.io", "library/nginx", "1.27"}},
		{in: "index.docker.io/acme/web:prod", want: Reference{"docker.io", "acme/web", "prod"}},
		{in: "ghcr.io/lemonity-org/app", wantErr: true},
		{in: "localhost:5000/app", wantErr: true},
		{in: "ghcr.io/lemonity-org/app@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{in: "ghcr.io/Lemonity/app:prod", wantErr: true},
		{in: "ghcr.io/lemonity-org/app:-bad", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseReference(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseReference(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseReference(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" {
		t.Fatalf("scheme = %q", scheme)
	}
	if params["realm"] != "https://ghcr.io/token" || params["service"] != "ghcr.io" || params["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("params = %v", params)
	}
	scheme, params = parseChallenge(`Basic realm=registry`)
	if scheme != "Basic" || params["realm"] != "registry" {
		t.Fatalf("basic challenge = %q %v", scheme, params)
	}
}
