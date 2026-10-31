package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrNotFound reports that the registry has no manifest or blob at that
// reference.
var ErrNotFound = errors.New("not found in registry")

// Manifest is a manifest exactly as the registry stored it. Its digest is the
// SHA-256 of Body, which is what signatures and tags refer to.
type Manifest struct {
	MediaType string
	Digest    string
	Body      []byte
}

// Repository reads and writes one repository's manifests and blobs. It keeps
// the authorization it earns, and asks for more when the registry challenges
// a request again (for example when an upload needs push access).
type Repository struct {
	resolver *Resolver
	ref      Reference
	creds    *Credentials
	auth     string
}

// Repository returns a client for registry/name. Credentials go only to that
// registry and its token service.
func (r *Resolver) Repository(registry, name string, creds *Credentials) *Repository {
	return &Repository{resolver: r, ref: Reference{Registry: registry, Repository: name}, creds: creds}
}

// Name returns the repository as registry/name.
func (repo *Repository) Name() string {
	return repo.ref.Registry + "/" + repo.ref.Repository
}

// Manifest fetches the manifest for a tag or digest. A manifest fetched by
// digest is checked against it.
func (repo *Repository) Manifest(ctx context.Context, reference string) (Manifest, error) {
	resp, err := repo.send(ctx, http.MethodGet, repo.url("manifests/"+url.PathEscape(reference)), map[string]string{"Accept": manifestAccept}, nil)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := repo.checkStatus(resp, "manifest "+reference, http.StatusOK); err != nil {
		return Manifest{}, err
	}
	body, err := readCapped(resp.Body, maxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("failed to read manifest %s in %s: %w", reference, repo.Name(), err)
	}
	digest := Digest(body)
	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != digest {
		return Manifest{}, fmt.Errorf("registry reported digest %s for manifest %s in %s but it hashes to %s", header, reference, repo.Name(), digest)
	}
	if digestRegex.MatchString(reference) && reference != digest {
		return Manifest{}, fmt.Errorf("manifest %s in %s hashes to %s", reference, repo.Name(), digest)
	}
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return Manifest{MediaType: strings.TrimSpace(mediaType), Digest: digest, Body: body}, nil
}

// Blob fetches a blob and checks it against its digest. Blobs larger than
// maxBytes are refused.
func (repo *Repository) Blob(ctx context.Context, digest string, maxBytes int64) ([]byte, error) {
	if !digestRegex.MatchString(digest) {
		return nil, fmt.Errorf("invalid blob digest %q", digest)
	}
	resp, err := repo.send(ctx, http.MethodGet, repo.url("blobs/"+digest), nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := repo.checkStatus(resp, "blob "+digest, http.StatusOK); err != nil {
		return nil, err
	}
	body, err := readCapped(resp.Body, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob %s in %s: %w", digest, repo.Name(), err)
	}
	if got := Digest(body); got != digest {
		return nil, fmt.Errorf("blob %s in %s hashes to %s", digest, repo.Name(), got)
	}
	return body, nil
}

// PushBlob uploads data unless the repository already has it, and returns its
// digest.
func (repo *Repository) PushBlob(ctx context.Context, data []byte) (string, error) {
	digest := Digest(data)
	resp, err := repo.send(ctx, http.MethodHead, repo.url("blobs/"+digest), nil, nil)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return digest, nil
	}

	resp, err = repo.send(ctx, http.MethodPost, repo.url("blobs/uploads/"), nil, nil)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if err := repo.checkStatus(resp, "blob upload", http.StatusAccepted); err != nil {
		return "", err
	}
	location, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return "", fmt.Errorf("registry %s started a blob upload without a usable location", repo.ref.Registry)
	}
	if location.Scheme != "https" {
		return "", fmt.Errorf("registry %s sent a blob upload location that is not HTTPS", repo.ref.Registry)
	}
	query := location.Query()
	query.Set("digest", digest)
	location.RawQuery = query.Encode()

	resp, err = repo.send(ctx, http.MethodPut, location.String(), map[string]string{"Content-Type": "application/octet-stream"}, data)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if err := repo.checkStatus(resp, "blob "+digest, http.StatusCreated); err != nil {
		return "", err
	}
	return digest, nil
}

// PutManifest stores a manifest under a tag or its own digest and returns its
// digest.
func (repo *Repository) PutManifest(ctx context.Context, reference, mediaType string, body []byte) (string, error) {
	digest := Digest(body)
	if digestRegex.MatchString(reference) && reference != digest {
		return "", fmt.Errorf("manifest hashes to %s, not %s", digest, reference)
	}
	if !digestRegex.MatchString(reference) && !tagRegex.MatchString(reference) {
		return "", fmt.Errorf("invalid tag %q", reference)
	}
	resp, err := repo.send(ctx, http.MethodPut, repo.url("manifests/"+url.PathEscape(reference)), map[string]string{"Content-Type": mediaType}, body)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if err := repo.checkStatus(resp, "manifest "+reference, http.StatusCreated, http.StatusOK); err != nil {
		return "", err
	}
	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != digest {
		return "", fmt.Errorf("registry stored manifest %s in %s as %s", digest, repo.Name(), header)
	}
	return digest, nil
}

// Digest returns the sha256 digest of data in OCI form.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidDigest reports whether s is a sha256 digest in OCI form.
func ValidDigest(s string) bool {
	return digestRegex.MatchString(s)
}

func (repo *Repository) url(path string) string {
	return fmt.Sprintf("https://%s/v2/%s/%s", repo.ref.apiHost(), repo.ref.Repository, path)
}

// send performs a request, answering one authentication challenge. Only an
// HTTPS request to the registry's own host gets the Authorization header or
// has its challenge answered, so an upload location or redirect elsewhere
// never receives a token, and cannot make Azud send the registry password to
// a token service of its choosing.
func (repo *Repository) send(ctx context.Context, method, target string, header map[string]string, body []byte) (*http.Response, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	trusted := parsed.Scheme == "https" && parsed.Host == repo.ref.apiHost()

	attempt := func() (*http.Response, error) {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}
		if repo.auth != "" && trusted {
			req.Header.Set("Authorization", repo.auth)
		}
		return repo.resolver.do(req)
	}

	resp, err := attempt()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !trusted {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	auth, err := repo.resolver.authorize(ctx, challenge, repo.ref, repo.creds)
	if err != nil {
		return nil, err
	}
	repo.auth = auth
	return attempt()
}

func (repo *Repository) checkStatus(resp *http.Response, what string, ok ...int) error {
	for _, code := range ok {
		if resp.StatusCode == code {
			return nil
		}
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s in %s", ErrNotFound, what, repo.Name())
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("registry denied access to %s in %s (HTTP %d)", what, repo.Name(), resp.StatusCode)
	default:
		return fmt.Errorf("registry returned HTTP %d for %s in %s", resp.StatusCode, what, repo.Name())
	}
}

func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return body, nil
}
