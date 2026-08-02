package deploy

import (
	"strings"
	"testing"
)

func TestStripImageTag(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"nginx", "nginx"},
		{"nginx:1.25", "nginx"},
		{"ghcr.io/org/app:v2", "ghcr.io/org/app"},
		{"ghcr.io/org/app@sha256:abcdef", "ghcr.io/org/app"},
		{"ghcr.io/org/app:v2@sha256:abcdef", "ghcr.io/org/app"},
		{"localhost:5000/img", "localhost:5000/img"}, // registry port, not a tag
		{"localhost:5000/img:v1", "localhost:5000/img"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := stripImageTag(tt.image); got != tt.want {
				t.Errorf("stripImageTag(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestHasImageTag(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"nginx", false},
		{"nginx:1.25", true},
		{"ghcr.io/org/app:v2", true},
		{"ghcr.io/org/app", false},
		{"localhost:5000/img", false}, // registry port, not a tag
		{"localhost:5000/img:v1", true},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := hasImageTag(tt.image); got != tt.want {
				t.Errorf("hasImageTag(%q) = %v, want %v", tt.image, got, tt.want)
			}
		})
	}
}

func TestImageReferenceForVersionPreservesTagAndDigestSemantics(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name    string
		image   string
		version string
		want    string
	}{
		{name: "tag replaces tag", image: "ghcr.io/acme/app:current", version: "v1.2.3", want: "ghcr.io/acme/app:v1.2.3"},
		{name: "tag replaces digest", image: "ghcr.io/acme/app@" + digest, version: "v1.2.3", want: "ghcr.io/acme/app:v1.2.3"},
		{name: "digest replaces tag", image: "ghcr.io/acme/app:current", version: digest, want: "ghcr.io/acme/app@" + digest},
		{name: "digest with registry port", image: "localhost:5000/acme/app:current", version: digest, want: "localhost:5000/acme/app@" + digest},
		{name: "other OCI digest algorithm", image: "ghcr.io/acme/app", version: "sha512:" + strings.Repeat("b", 128), want: "ghcr.io/acme/app@sha512:" + strings.Repeat("b", 128)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImageReferenceForVersion(tt.image, tt.version); got != tt.want {
				t.Fatalf("ImageReferenceForVersion(%q, %q) = %q, want %q", tt.image, tt.version, got, tt.want)
			}
		})
	}
}

func TestResolveDeploymentImageSupportsDigestHistoryRollback(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, tt := range []struct {
		name        string
		configured  string
		requested   string
		wantImage   string
		wantVersion string
	}{
		{name: "old history digest", configured: "ghcr.io/acme/app:latest", requested: digest, wantImage: "ghcr.io/acme/app@" + digest, wantVersion: digest},
		{name: "configured digest", configured: "ghcr.io/acme/app:v1@" + digest, wantImage: "ghcr.io/acme/app:v1@" + digest, wantVersion: digest},
		{name: "configured tag", configured: "ghcr.io/acme/app:v1", wantImage: "ghcr.io/acme/app:v1", wantVersion: "v1"},
		{name: "implicit latest", configured: "ghcr.io/acme/app", wantImage: "ghcr.io/acme/app:latest", wantVersion: "latest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			image, version := resolveDeploymentImage(tt.configured, tt.requested)
			if image != tt.wantImage || version != tt.wantVersion {
				t.Fatalf("resolveDeploymentImage(%q, %q) = (%q, %q), want (%q, %q)", tt.configured, tt.requested, image, version, tt.wantImage, tt.wantVersion)
			}
			if strings.Contains(image, ":sha256:") {
				t.Fatalf("resolved malformed digest-as-tag reference %q", image)
			}
		})
	}
}

func TestIsDigestVersionRejectsTagsAndMalformedDigests(t *testing.T) {
	for _, value := range []string{
		"sha256:abc123",
		"sha512:ABC_def-123=",
		"1sha:abc",
		"multihash+base58:QmYwAPJzv5CZsnAzt8auVZRnGi",
	} {
		if !isDigestVersion(value) {
			t.Fatalf("valid digest-shaped version %q rejected", value)
		}
	}
	for _, value := range []string{
		"latest",
		"v1.2.3",
		":abc",
		"sha256:",
		"SHA256:abc",
		"sha..256:abc",
		"sha256+:abc",
		"sha256:abc/def",
	} {
		if isDigestVersion(value) {
			t.Fatalf("non-digest version %q accepted", value)
		}
	}
}
