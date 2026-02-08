package podman

import "testing"

func TestParseHostPort(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "ipv4",
			input:    "127.0.0.1:8080",
			wantHost: "127.0.0.1",
			wantPort: 8080,
		},
		{
			name:     "ipv6",
			input:    "[::1]:8443",
			wantHost: "::1",
			wantPort: 8443,
		},
		{
			name:     "wildcard",
			input:    "0.0.0.0:49123",
			wantHost: "0.0.0.0",
			wantPort: 49123,
		},
		{
			name:    "missing separator",
			input:   "8080",
			wantErr: true,
		},
		{
			name:    "invalid port",
			input:   "127.0.0.1:not-a-port",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseHostPort(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q", tt.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost {
				t.Fatalf("unexpected host: want %q got %q", tt.wantHost, host)
			}
			if port != tt.wantPort {
				t.Fatalf("unexpected port: want %d got %d", tt.wantPort, port)
			}
		})
	}
}
