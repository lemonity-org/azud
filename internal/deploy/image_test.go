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
