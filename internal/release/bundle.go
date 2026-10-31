// Package release builds, publishes and reads release bundles: signed OCI
// artifacts that carry everything a pull-mode server applies for one release.
// A bundle names the image by digest, carries the configuration files as
// written in the repository, and the runtime secrets file encrypted with age
// to the servers' recipients. See docs/PULL_MODE.md.
package release

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/lemonity-org/azud/internal/registry"
)

// Media types of a release bundle.
const (
	ArtifactType     = "application/vnd.lemonity.azud.release.v1"
	InfoMediaType    = "application/vnd.lemonity.azud.release.config.v1+json"
	ConfigMediaType  = "application/vnd.lemonity.azud.release.deploy.v1+yaml"
	SecretsMediaType = "application/vnd.lemonity.azud.release.secrets.v1+age"

	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	titleAnnotation   = "org.opencontainers.image.title"
	// SecretsFile is the title of the encrypted secrets layer.
	SecretsFile = "secrets.age"
)

// Limits on what a bundle may contain, checked when it is built and read.
const (
	maxInfoBytes   = 64 << 10
	MaxConfigBytes = 1 << 20
	maxLayers      = 3
	formatVersion  = 1
)

// configNameRegex matches the configuration file names a bundle carries:
// deploy.yml and deploy.<destination>.yml.
var configNameRegex = regexp.MustCompile(`^deploy(\.[a-zA-Z0-9][a-zA-Z0-9_-]{0,62})?\.yml$`)

// Info is release.json, the bundle's config blob.
type Info struct {
	Format int `json:"format"`
	// Service is the azud service the release belongs to.
	Service string `json:"service"`
	// Repository is the image repository, registry/name. The bundle lives
	// in the same repository.
	Repository string `json:"repository"`
	// Image is the digest of the image manifest the release deploys.
	Image string `json:"image"`
	// Destination is the destination whose file the bundle carries, if any.
	Destination string    `json:"destination,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Created     time.Time `json:"created"`
	// Secrets lists the names of the secrets in the encrypted file.
	Secrets []string `json:"secrets,omitempty"`
	// Recipients lists the age recipients the secrets are encrypted to.
	Recipients []string `json:"recipients,omitempty"`
}

// File is a configuration file carried as written.
type File struct {
	Name string
	Data []byte
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Bundle is a built release, ready to publish.
type Bundle struct {
	Digest   string
	Manifest []byte
	blobs    [][]byte
}

// Release is a bundle read back from a registry. Secrets stays encrypted.
type Release struct {
	Digest  string
	Info    Info
	Files   []File
	Secrets []byte
}

// Build assembles a bundle. image describes the image manifest (its media
// type, digest and size), which becomes the bundle's subject. secrets is the
// encrypted secrets file, or nil when the release has none.
func Build(info Info, image Descriptor, files []File, secrets []byte) (*Bundle, error) {
	info.Format = formatVersion
	if err := validateInfo(info); err != nil {
		return nil, err
	}
	if image.Digest != info.Image {
		return nil, fmt.Errorf("image descriptor %s does not match release image %s", image.Digest, info.Image)
	}
	if len(files) == 0 || files[0].Name != "deploy.yml" {
		return nil, fmt.Errorf("a release carries deploy.yml first")
	}
	if len(files)+1 > maxLayers {
		return nil, fmt.Errorf("a release carries deploy.yml and at most one destination file")
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	b := &Bundle{blobs: [][]byte{infoJSON}}
	m := manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		ArtifactType:  ArtifactType,
		Config:        descriptor(InfoMediaType, infoJSON, ""),
		Subject:       &Descriptor{MediaType: image.MediaType, Digest: image.Digest, Size: image.Size},
		Annotations:   map[string]string{"org.opencontainers.image.created": info.Created.UTC().Format(time.RFC3339)},
	}
	if info.Commit != "" {
		m.Annotations["org.opencontainers.image.revision"] = info.Commit
	}
	seen := map[string]bool{}
	for _, f := range files {
		if !configNameRegex.MatchString(f.Name) || seen[f.Name] {
			return nil, fmt.Errorf("invalid or repeated configuration file name %q", f.Name)
		}
		if len(f.Data) > MaxConfigBytes {
			return nil, fmt.Errorf("%s exceeds %d bytes", f.Name, MaxConfigBytes)
		}
		seen[f.Name] = true
		m.Layers = append(m.Layers, descriptor(ConfigMediaType, f.Data, f.Name))
		b.blobs = append(b.blobs, f.Data)
	}
	if secrets != nil {
		if len(secrets) > MaxSecretsBytes {
			return nil, fmt.Errorf("encrypted secrets exceed %d bytes", MaxSecretsBytes)
		}
		m.Layers = append(m.Layers, descriptor(SecretsMediaType, secrets, SecretsFile))
		b.blobs = append(b.blobs, secrets)
	}

	b.Manifest, err = json.Marshal(m)
	if err != nil {
		return nil, err
	}
	b.Digest = registry.Digest(b.Manifest)
	return b, nil
}

// Publish uploads the bundle's blobs and its manifest by digest. It never
// tags: the tag moves only after the bundle is signed (Promote).
func Publish(ctx context.Context, repo *registry.Repository, b *Bundle) error {
	for _, blob := range b.blobs {
		if _, err := repo.PushBlob(ctx, blob); err != nil {
			return err
		}
	}
	_, err := repo.PutManifest(ctx, b.Digest, manifestMediaType, b.Manifest)
	return err
}

// Promote points tag at the bundle with the given digest, after checking that
// the digest names a release bundle of service.
func Promote(ctx context.Context, repo *registry.Repository, digest, tag, service string) error {
	m, err := repo.Manifest(ctx, digest)
	if err != nil {
		return err
	}
	parsed, err := parseManifest(m)
	if err != nil {
		return err
	}
	infoJSON, err := repo.Blob(ctx, parsed.Config.Digest, maxInfoBytes)
	if err != nil {
		return err
	}
	info, err := parseInfo(infoJSON)
	if err != nil {
		return err
	}
	if info.Service != service {
		return fmt.Errorf("%s is a release of %q, not %q", digest, info.Service, service)
	}
	_, err = repo.PutManifest(ctx, tag, manifestMediaType, m.Body)
	return err
}

// Fetch reads the bundle with the given digest and checks it: an azud
// release manifest, only known layers, digests and sizes as declared, and a
// release.json that matches the repository it was read from.
func Fetch(ctx context.Context, repo *registry.Repository, digest string) (*Release, error) {
	if !registry.ValidDigest(digest) {
		return nil, fmt.Errorf("invalid release digest %q", digest)
	}
	m, err := repo.Manifest(ctx, digest)
	if err != nil {
		return nil, err
	}
	parsed, err := parseManifest(m)
	if err != nil {
		return nil, err
	}
	if parsed.Config.Size > maxInfoBytes {
		return nil, fmt.Errorf("release %s: release.json exceeds %d bytes", digest, maxInfoBytes)
	}
	infoJSON, err := repo.Blob(ctx, parsed.Config.Digest, parsed.Config.Size)
	if err != nil {
		return nil, err
	}
	info, err := parseInfo(infoJSON)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", digest, err)
	}
	if info.Repository != repo.Name() {
		return nil, fmt.Errorf("release %s belongs to %s, not %s", digest, info.Repository, repo.Name())
	}
	if parsed.Subject == nil || parsed.Subject.Digest != info.Image {
		return nil, fmt.Errorf("release %s: subject does not match release image %s", digest, info.Image)
	}

	rel := &Release{Digest: digest, Info: info}
	seen := map[string]bool{}
	for _, layer := range parsed.Layers {
		title := layer.Annotations[titleAnnotation]
		if seen[title] {
			return nil, fmt.Errorf("release %s: repeated layer %q", digest, title)
		}
		seen[title] = true
		limit := int64(MaxConfigBytes)
		if layer.MediaType == SecretsMediaType {
			limit = MaxSecretsBytes
		}
		if layer.Size > limit {
			return nil, fmt.Errorf("release %s: %s exceeds %d bytes", digest, title, limit)
		}
		data, err := repo.Blob(ctx, layer.Digest, layer.Size)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != layer.Size {
			return nil, fmt.Errorf("release %s: %s is %d bytes, not the declared %d", digest, title, len(data), layer.Size)
		}
		if layer.MediaType == SecretsMediaType {
			rel.Secrets = data
		} else {
			rel.Files = append(rel.Files, File{Name: title, Data: data})
		}
	}
	if len(rel.Files) == 0 || rel.Files[0].Name != "deploy.yml" {
		return nil, fmt.Errorf("release %s does not start with deploy.yml", digest)
	}
	if len(info.Secrets) > 0 && rel.Secrets == nil {
		return nil, fmt.Errorf("release %s lists secrets but carries no secrets layer", digest)
	}
	return rel, nil
}

func parseManifest(m registry.Manifest) (*manifest, error) {
	var parsed manifest
	dec := json.NewDecoder(bytes.NewReader(m.Body))
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%s is not a manifest: %w", m.Digest, err)
	}
	if parsed.MediaType != manifestMediaType || parsed.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("%s is not an azud release bundle (artifact type %q)", m.Digest, parsed.ArtifactType)
	}
	if parsed.Config.MediaType != InfoMediaType || !registry.ValidDigest(parsed.Config.Digest) {
		return nil, fmt.Errorf("%s has an unexpected config %q", m.Digest, parsed.Config.MediaType)
	}
	if len(parsed.Layers) == 0 || len(parsed.Layers) > maxLayers {
		return nil, fmt.Errorf("%s has %d layers; a release has 1 to %d", m.Digest, len(parsed.Layers), maxLayers)
	}
	for _, layer := range parsed.Layers {
		title := layer.Annotations[titleAnnotation]
		switch {
		case !registry.ValidDigest(layer.Digest) || layer.Size < 0:
			return nil, fmt.Errorf("%s has a malformed layer descriptor", m.Digest)
		case layer.MediaType == ConfigMediaType && configNameRegex.MatchString(title):
		case layer.MediaType == SecretsMediaType && title == SecretsFile:
		default:
			return nil, fmt.Errorf("%s has an unexpected layer %q (%s)", m.Digest, title, layer.MediaType)
		}
	}
	return &parsed, nil
}

func parseInfo(data []byte) (Info, error) {
	var info Info
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		return Info{}, fmt.Errorf("unreadable release.json: %w", err)
	}
	if info.Format != formatVersion {
		return Info{}, fmt.Errorf("release format %d is not supported by this azud (supports %d)", info.Format, formatVersion)
	}
	return info, validateInfo(info)
}

func validateInfo(info Info) error {
	switch {
	case info.Service == "":
		return fmt.Errorf("release.json has no service")
	case info.Repository == "":
		return fmt.Errorf("release.json has no repository")
	case !registry.ValidDigest(info.Image):
		return fmt.Errorf("release.json has an invalid image digest %q", info.Image)
	case info.Created.IsZero():
		return fmt.Errorf("release.json has no creation time")
	case !slices.IsSorted(info.Secrets):
		return fmt.Errorf("release.json lists secrets out of order")
	}
	return nil
}

func descriptor(mediaType string, data []byte, title string) Descriptor {
	d := Descriptor{MediaType: mediaType, Digest: registry.Digest(data), Size: int64(len(data))}
	if title != "" {
		d.Annotations = map[string]string{titleAnnotation: title}
	}
	return d
}
