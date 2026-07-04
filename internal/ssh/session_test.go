package ssh

import "testing"

func TestQuoteRemotePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "root absolute path",
			path: "/var/lib/azud/svc.deploy.lock",
			want: "/var/lib/azud/svc.deploy.lock",
		},
		{
			name: "home path expands but remainder is quoted safely",
			path: "${HOME}/.local/share/azud/svc.deploy.lock",
			want: `"${HOME}/".local/share/azud/svc.deploy.lock`,
		},
		{
			name: "home path with unsafe remainder is single-quoted",
			path: "${HOME}/.local/share/azud/svc$(touch x).lock",
			want: `"${HOME}/"'.local/share/azud/svc$(touch x).lock'`,
		},
		{
			name: "absolute path with metacharacters is single-quoted",
			path: "/var/lib/azud/svc$(id).lock",
			want: `'/var/lib/azud/svc$(id).lock'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteRemotePath(tt.path); got != tt.want {
				t.Errorf("quoteRemotePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
