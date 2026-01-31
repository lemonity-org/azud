package deploy

import (
	"testing"
)

func TestFormatSecretErrors(t *testing.T) {
	tests := []struct {
		name         string
		missingByHost map[string][]string
		emptyByHost   map[string][]string
		wantMsg      string
	}{
		{
			name: "only missing keys on one host",
			missingByHost: map[string][]string{
				"10.0.0.1": {"API_KEY", "DB_PASS"},
			},
			emptyByHost: map[string][]string{},
			wantMsg:     "missing required secrets on host(s): 10.0.0.1 (API_KEY, DB_PASS) (update local secrets and run 'azud env push')",
		},
		{
			name:          "only empty keys on one host",
			missingByHost: map[string][]string{},
			emptyByHost: map[string][]string{
				"10.0.0.1": {"EMAIL_API_KEY"},
			},
			wantMsg: "empty required secrets on host(s): 10.0.0.1 (EMAIL_API_KEY) (update local secrets and run 'azud env push')",
		},
		{
			name: "both missing and empty on same host",
			missingByHost: map[string][]string{
				"10.0.0.1": {"FOO"},
			},
			emptyByHost: map[string][]string{
				"10.0.0.1": {"EMAIL_API_KEY"},
			},
			wantMsg: "secret issues on host(s): 10.0.0.1 (missing: FOO; empty: EMAIL_API_KEY) (update local secrets and run 'azud env push')",
		},
		{
			name: "missing on one host empty on another",
			missingByHost: map[string][]string{
				"10.0.0.1": {"FOO"},
			},
			emptyByHost: map[string][]string{
				"10.0.0.2": {"BAR"},
			},
			wantMsg: "secret issues on host(s): 10.0.0.1 (missing: FOO); 10.0.0.2 (empty: BAR) (update local secrets and run 'azud env push')",
		},
		{
			name: "multiple hosts only missing",
			missingByHost: map[string][]string{
				"10.0.0.1": {"A"},
				"10.0.0.2": {"B", "C"},
			},
			emptyByHost: map[string][]string{},
			wantMsg:     "missing required secrets on host(s): 10.0.0.1 (A); 10.0.0.2 (B, C) (update local secrets and run 'azud env push')",
		},
		{
			name:          "multiple hosts only empty",
			missingByHost: map[string][]string{},
			emptyByHost: map[string][]string{
				"10.0.0.1": {"X"},
				"10.0.0.2": {"Y"},
			},
			wantMsg: "empty required secrets on host(s): 10.0.0.1 (X); 10.0.0.2 (Y) (update local secrets and run 'azud env push')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := formatSecretErrors(tt.missingByHost, tt.emptyByHost)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.wantMsg {
				t.Errorf("got:\n  %s\nwant:\n  %s", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestParseSecretsContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
	}{
		{
			name:    "key with value",
			content: "API_KEY=secret123",
			want:    map[string]string{"API_KEY": "secret123"},
		},
		{
			name:    "key with empty value",
			content: "API_KEY=",
			want:    map[string]string{"API_KEY": ""},
		},
		{
			name:    "key not present",
			content: "OTHER_KEY=value",
			want:    map[string]string{"OTHER_KEY": "value"},
		},
		{
			name:    "export prefix",
			content: "export FOO=bar",
			want:    map[string]string{"FOO": "bar"},
		},
		{
			name:    "comments and blanks ignored",
			content: "# comment\n\nKEY=val\n",
			want:    map[string]string{"KEY": "val"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSecretsContent(tt.content)
			if len(got) != len(tt.want) {
				t.Errorf("got %d keys, want %d", len(got), len(tt.want))
			}
			for k, wantV := range tt.want {
				gotV, ok := got[k]
				if !ok {
					t.Errorf("missing key %q", k)
				} else if gotV != wantV {
					t.Errorf("key %q: got %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}

func TestNormalizeSecretKeys(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want []string
	}{
		{
			name: "dedup and sort",
			keys: []string{"B", "A", "B", "C"},
			want: []string{"A", "B", "C"},
		},
		{
			name: "trim whitespace and skip empty",
			keys: []string{" FOO ", "", "  ", "BAR"},
			want: []string{"BAR", "FOO"},
		},
		{
			name: "empty input",
			keys: []string{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeSecretKeys(tt.keys)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
