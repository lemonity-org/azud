package deploy

import "testing"

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

func TestImageAtVersion(t *testing.T) {
	const digest = "sha256:4f2a5e3c9b1d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f"
	tests := []struct {
		image   string
		version string
		want    string
	}{
		{"ghcr.io/org/app", "v2", "ghcr.io/org/app:v2"},
		{"ghcr.io/org/app:v1", "v2", "ghcr.io/org/app:v2"},
		{"ghcr.io/org/app", digest, "ghcr.io/org/app@" + digest},
		{"ghcr.io/org/app@sha256:abcdef", digest, "ghcr.io/org/app@" + digest},
		{"ghcr.io/org/app:v1", digest, "ghcr.io/org/app@" + digest},
		{"localhost:5000/img", "v1", "localhost:5000/img:v1"},
		{"localhost:5000/img", digest, "localhost:5000/img@" + digest},
	}
	for _, tt := range tests {
		t.Run(tt.image+" "+tt.version, func(t *testing.T) {
			if got := ImageAtVersion(tt.image, tt.version); got != tt.want {
				t.Errorf("ImageAtVersion(%q, %q) = %q, want %q", tt.image, tt.version, got, tt.want)
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
