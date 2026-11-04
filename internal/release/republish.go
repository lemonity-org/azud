package release

import (
	"context"
	"fmt"
	"time"

	"github.com/lemonity-org/azud/internal/registry"
)

// RepublishedFromAnnotation is the manifest annotation naming the bundle a republished
// bundle was copied from. Provenance is here and not in release.json because parseInfo
// refuses a field it does not know, so a new one would make a v1 agent refuse the bundle.
const RepublishedFromAnnotation = "com.lemonity.azud.release.republished-from"

// Republish serves `azud release republish`, a rollback with no access to the server. It
// publishes a new bundle from the one digest names: the same image, the same configuration
// files, the same encrypted secrets byte for byte, and created as its creation time. It
// needs no secrets and decrypts nothing. The new creation time passes the downgrade check,
// and the new digest must be signed before it is promoted, so a person who can only move
// the tag still cannot bring back an old release. It never tags.
func Republish(ctx context.Context, repo *registry.Repository, digest, service string, created time.Time) (*Bundle, error) {
	rel, err := Fetch(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	if rel.Info.Service != service {
		return nil, fmt.Errorf("%s is a release of %q, not %q", digest, rel.Info.Service, service)
	}
	// A promoted bundle whose image has gone would fail only on the server.
	if _, err := repo.Manifest(ctx, rel.Info.Image); err != nil {
		return nil, fmt.Errorf("image %s of this release: %w", rel.Info.Image, err)
	}
	info := rel.Info
	info.Created = created.UTC()
	bundle, err := build(info, rel.Subject, rel.Files, rel.Secrets, map[string]string{RepublishedFromAnnotation: digest})
	if err != nil {
		return nil, err
	}
	if err := Publish(ctx, repo, bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}
