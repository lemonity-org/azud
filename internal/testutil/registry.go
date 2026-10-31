package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Registry is an in-memory OCI distribution registry over TLS, for tests. Like
// ghcr.io, every request needs a bearer token from /token: reads need pull
// scope and writes need push scope, and /token wants basic credentials.
type Registry struct {
	Server   *httptest.Server
	Username string
	Password string

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]storedManifest // by digest
	tags      map[string]string         // repository:tag -> digest
	uploads   int
	// Scopes records the scope of every token request.
	Scopes []string
	// Requests records "METHOD path" for every distribution API request.
	Requests []string
}

type storedManifest struct {
	mediaType string
	body      []byte
}

// NewRegistry starts a registry that accepts the given credentials.
func NewRegistry(t *testing.T, username, password string) *Registry {
	t.Helper()
	r := &Registry{
		Username:  username,
		Password:  password,
		blobs:     make(map[string][]byte),
		manifests: make(map[string]storedManifest),
		tags:      make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", r.token)
	mux.HandleFunc("/v2/", r.distribution)
	r.Server = httptest.NewTLSServer(mux)
	t.Cleanup(r.Server.Close)
	return r
}

// Host returns the registry's host:port, as it appears in image names.
func (r *Registry) Host() string {
	return r.Server.Listener.Addr().String()
}

// Client returns an HTTP client that trusts the registry's certificate.
func (r *Registry) Client() *http.Client {
	return r.Server.Client()
}

// PutManifest stores a manifest directly, optionally under a tag, and returns
// its digest.
func (r *Registry) PutManifest(repository, tag, mediaType string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	digest := sha256Digest(body)
	r.manifests[digest] = storedManifest{mediaType: mediaType, body: body}
	if tag != "" {
		r.tags[repository+":"+tag] = digest
	}
	return digest
}

// Tag returns the digest a tag points to, or "".
func (r *Registry) Tag(repository, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tags[repository+":"+tag]
}

// Blob returns a stored blob.
func (r *Registry) Blob(digest string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blobs[digest]
	return b, ok
}

// SetBlob stores or replaces a blob without checking its digest, to simulate
// a registry that serves the wrong content.
func (r *Registry) SetBlob(digest string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[digest] = data
}

func (r *Registry) token(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	scope := req.URL.Query().Get("scope")
	r.Scopes = append(r.Scopes, scope)
	r.mu.Unlock()
	user, pass, ok := req.BasicAuth()
	if !ok || user != r.Username || pass != r.Password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, _ = fmt.Fprintf(w, `{"token":%q}`, "scope="+scope)
}

func (r *Registry) distribution(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.Requests = append(r.Requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()

	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	var repository, kind, rest string
	for _, marker := range []string{"/manifests/", "/blobs/uploads/", "/blobs/"} {
		if before, after, found := strings.Cut(path, marker); found {
			repository, kind, rest = before, strings.Trim(marker, "/"), after
			break
		}
	}
	if repository == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	write := req.Method == http.MethodPut || req.Method == http.MethodPost
	actions := "pull"
	if write {
		actions = "pull,push"
	}
	granted := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer scope=")
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer scope=") ||
		!scopeAllows(granted, repository, write) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:%s:%s"`, r.Server.URL, repository, actions))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch kind {
	case "manifests":
		r.serveManifest(w, req, repository, rest)
	case "blobs/uploads":
		r.serveUpload(w, req, repository, rest)
	default:
		r.serveBlob(w, req, rest)
	}
}

func scopeAllows(granted, repository string, write bool) bool {
	prefix := "repository:" + repository + ":"
	if !strings.HasPrefix(granted, prefix) {
		return false
	}
	actions := strings.Split(strings.TrimPrefix(granted, prefix), ",")
	need := "pull"
	if write {
		need = "push"
	}
	for _, action := range actions {
		if action == need {
			return true
		}
	}
	return false
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, repository, reference string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		digest := reference
		if !strings.HasPrefix(reference, "sha256:") {
			digest = r.tags[repository+":"+reference]
		}
		m, ok := r.manifests[digest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		if req.Method == http.MethodGet {
			_, _ = w.Write(m.body)
		}
	case http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		digest := sha256Digest(body)
		if strings.HasPrefix(reference, "sha256:") && reference != digest {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.manifests[digest] = storedManifest{mediaType: req.Header.Get("Content-Type"), body: body}
		if !strings.HasPrefix(reference, "sha256:") {
			r.tags[repository+":"+reference] = digest
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := r.blobs[digest]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if req.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (r *Registry) serveUpload(w http.ResponseWriter, req *http.Request, repository, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case req.Method == http.MethodPost && id == "":
		r.uploads++
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%d", repository, r.uploads))
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && id != "":
		body, _ := io.ReadAll(req.Body)
		digest := req.URL.Query().Get("digest")
		if sha256Digest(body) != digest {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.blobs[digest] = body
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
