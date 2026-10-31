// Package registry resolves image tags to manifest digests through the OCI
// distribution API, without pulling the image. `azud agent` uses it to notice
// when the tag it follows moves to a new release.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrTagNotFound reports that the registry has no manifest for the tag.
var ErrTagNotFound = errors.New("tag not found in registry")

// manifestAccept lists the manifest media types Azud deploys: OCI and Docker
// images and multi-platform indexes. The digest of whichever one the registry
// returns is what Podman pulls by.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// maxManifestBytes caps a manifest read when the registry omits the digest
// header and Azud has to hash the body itself.
const maxManifestBytes = 4 << 20

var (
	digestRegex        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagRegex           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	pathComponentRegex = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
)

// Reference names one tag in one repository.
type Reference struct {
	// Registry is the registry host as written in the image name, such as
	// ghcr.io or localhost:5000. Docker Hub images use docker.io.
	Registry string
	// Repository is the path inside the registry, such as org/app.
	Repository string
	// Tag is the tag to resolve.
	Tag string
}

// String returns the reference as registry/repository:tag.
func (r Reference) String() string {
	return r.Registry + "/" + r.Repository + ":" + r.Tag
}

// apiHost is the host serving the distribution API for the registry.
func (r Reference) apiHost() string {
	if r.Registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return r.Registry
}

// ParseReference parses an image reference that names a tag. Digest
// references are rejected: there is nothing to resolve.
func ParseReference(ref string) (Reference, error) {
	if strings.Contains(ref, "@") {
		return Reference{}, fmt.Errorf("%q is pinned to a digest; a tag is required", ref)
	}

	name, tag := ref, ""
	if idx := strings.LastIndexByte(ref, ':'); idx > strings.LastIndexByte(ref, '/') {
		name, tag = ref[:idx], ref[idx+1:]
	}
	if tag == "" {
		return Reference{}, fmt.Errorf("%q has no tag", ref)
	}
	if !tagRegex.MatchString(tag) {
		return Reference{}, fmt.Errorf("%q has an invalid tag", ref)
	}

	registry := "docker.io"
	components := strings.Split(name, "/")
	if len(components) > 1 && (strings.ContainsAny(components[0], ".:[") || components[0] == "localhost") {
		registry = components[0]
		components = components[1:]
	}
	if registry == "index.docker.io" || registry == "registry-1.docker.io" {
		registry = "docker.io"
	}
	if registry == "docker.io" && len(components) == 1 {
		components = append([]string{"library"}, components...)
	}
	for _, component := range components {
		if !pathComponentRegex.MatchString(component) {
			return Reference{}, fmt.Errorf("%q has an invalid repository path", ref)
		}
	}

	return Reference{Registry: registry, Repository: strings.Join(components, "/"), Tag: tag}, nil
}

// Credentials authenticate to a registry. Leave them nil for public images.
type Credentials struct {
	Username string
	Password string
}

// Resolver looks up manifest digests over HTTPS.
type Resolver struct {
	// Client sends the requests. Nil uses a client with a 30 second timeout.
	Client *http.Client
	// UserAgent identifies Azud to the registry.
	UserAgent string
}

// Digest returns the digest of the manifest ref's tag points to now.
func (r *Resolver) Digest(ctx context.Context, ref Reference, creds *Credentials) (string, error) {
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.apiHost(), ref.Repository, url.PathEscape(ref.Tag))

	authorization := ""
	resp, err := r.manifestRequest(ctx, http.MethodHead, manifestURL, authorization)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		authorization, err = r.authorize(ctx, challenge, ref, creds)
		if err != nil {
			return "", err
		}
		resp, err = r.manifestRequest(ctx, http.MethodHead, manifestURL, authorization)
		if err != nil {
			return "", err
		}
	}

	digest, err := digestFromResponse(resp, ref)
	_ = resp.Body.Close()
	if err == nil && digest != "" {
		return digest, nil
	}
	if err != nil && !errors.Is(err, errHeadUnsupported) {
		return "", err
	}

	// Some registries answer HEAD without the digest header, or not at all.
	// Fetch the manifest and hash it, which is how the digest is defined.
	resp, err = r.manifestRequest(ctx, http.MethodGet, manifestURL, authorization)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := digestFromResponse(resp, ref); err != nil && !errors.Is(err, errHeadUnsupported) {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read manifest for %s: %w", ref, err)
	}
	if len(body) > maxManifestBytes {
		return "", fmt.Errorf("manifest for %s exceeds %d bytes", ref, maxManifestBytes)
	}
	sum := sha256.Sum256(body)
	computed := "sha256:" + hex.EncodeToString(sum[:])
	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != computed {
		return "", fmt.Errorf("registry reported digest %s for %s but the manifest hashes to %s", header, ref, computed)
	}
	return computed, nil
}

var errHeadUnsupported = errors.New("registry did not report a digest")

// digestFromResponse checks the status of a manifest response and returns the
// digest header when present and well formed.
func digestFromResponse(resp *http.Response, ref Reference) (string, error) {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrTagNotFound, ref)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("registry denied access to %s (HTTP %d)", ref, resp.StatusCode)
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return "", errHeadUnsupported
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", fmt.Errorf("registry returned HTTP %d for %s", resp.StatusCode, ref)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", errHeadUnsupported
	}
	if !digestRegex.MatchString(digest) {
		return "", fmt.Errorf("registry returned malformed digest %q for %s", digest, ref)
	}
	return digest, nil
}

func (r *Resolver) manifestRequest(ctx context.Context, method, manifestURL, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return r.do(req)
}

func (r *Resolver) do(req *http.Request) (*http.Response, error) {
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", err)
	}
	return resp, nil
}

// authorize answers a WWW-Authenticate challenge with an Authorization header
// value: a bearer token from the registry's token service, or basic auth.
func (r *Resolver) authorize(ctx context.Context, challenge string, ref Reference, creds *Credentials) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if creds == nil {
			return "", fmt.Errorf("registry %s requires credentials", ref.Registry)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password)), nil
	case "bearer":
		return r.bearerToken(ctx, params, ref, creds)
	default:
		return "", fmt.Errorf("registry %s sent an unsupported authentication challenge %q", ref.Registry, scheme)
	}
}

func (r *Resolver) bearerToken(ctx context.Context, params map[string]string, ref Reference, creds *Credentials) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("registry %s sent a token challenge without a usable realm", ref.Registry)
	}
	// Credentials and tokens only ever travel over TLS.
	if realm.Scheme != "https" {
		return "", fmt.Errorf("registry %s token service %s is not HTTPS", ref.Registry, realm.Redacted())
	}

	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := r.do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s token service returned HTTP %d", ref.Registry, resp.StatusCode)
	}

	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return "", fmt.Errorf("registry %s token service returned an unreadable token: %w", ref.Registry, err)
	}
	value := token.Token
	if value == "" {
		value = token.AccessToken
	}
	if value == "" {
		return "", fmt.Errorf("registry %s token service returned no token", ref.Registry)
	}
	return "Bearer " + value, nil
}

// parseChallenge splits a WWW-Authenticate value such as
// `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/r:pull"`
// into its scheme and parameters. Quoted values may contain commas.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	scheme, rest, _ := strings.Cut(header, " ")
	params := make(map[string]string)
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var value string
		if strings.HasPrefix(after, `"`) {
			end := 1
			var builder strings.Builder
			for end < len(after) && after[end] != '"' {
				if after[end] == '\\' && end+1 < len(after) {
					end++
				}
				builder.WriteByte(after[end])
				end++
			}
			value = builder.String()
			if end < len(after) {
				end++
			}
			after = after[end:]
		} else {
			value, after, _ = strings.Cut(after, ",")
			after = "," + after
		}
		params[key] = strings.TrimSpace(value)
		after = strings.TrimSpace(after)
		after = strings.TrimPrefix(after, ",")
		rest = strings.TrimSpace(after)
	}
	return scheme, params
}
