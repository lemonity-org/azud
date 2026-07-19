package deploy

import (
	"errors"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

func TestVerifyImageDigestFailsClosed(t *testing.T) {
	d := &Deployer{
		cfg: &config.Config{},
		log: output.DefaultLogger,
		imageDigest: func(host, image string) (string, error) {
			return "", errors.New("no repo digest")
		},
	}

	_, err := d.verifyImageDigest([]string{"one"}, "ghcr.io/acme/app:latest")
	if err == nil || !strings.Contains(err.Error(), "failed to get image digest on one") {
		t.Fatalf("expected a fail-closed digest error, got %v", err)
	}
}

func TestVerifyImageDigestExplicitBypass(t *testing.T) {
	d := &Deployer{
		cfg: &config.Config{Deploy: config.DeployConfig{AllowUnverifiedImage: true}},
		log: output.DefaultLogger,
		imageDigest: func(host, image string) (string, error) {
			return "", errors.New("local image has no repo digest")
		},
	}

	digest, err := d.verifyImageDigest([]string{"one"}, "localhost/app:dev")
	if err != nil || digest != "" {
		t.Fatalf("explicit bypass = (%q, %v), want empty digest and nil error", digest, err)
	}
}

func TestVerifyImageDigestAcrossHosts(t *testing.T) {
	digests := map[string]string{"one": "sha256:aaa", "two": "sha256:aaa"}
	d := &Deployer{
		cfg: &config.Config{},
		log: output.DefaultLogger,
		imageDigest: func(host, image string) (string, error) {
			return digests[host], nil
		},
	}

	digest, err := d.verifyImageDigest([]string{"one", "two"}, "ghcr.io/acme/app@sha256:aaa")
	if err != nil || digest != "sha256:aaa" {
		t.Fatalf("matching digests = (%q, %v)", digest, err)
	}

	digests["two"] = "sha256:bbb"
	_, err = d.verifyImageDigest([]string{"one", "two"}, "ghcr.io/acme/app:latest")
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected mismatch, got %v", err)
	}
}
