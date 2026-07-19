package cli

import "testing"

func TestLingerCommandUsesNonInteractiveSudoForNonRoot(t *testing.T) {
	if got, want := lingerCommand("deploy"), "sudo -n loginctl enable-linger deploy"; got != want {
		t.Fatalf("linger command = %q, want %q", got, want)
	}
	if got, want := lingerCommand("root"), "loginctl enable-linger root"; got != want {
		t.Fatalf("root linger command = %q, want %q", got, want)
	}
}
