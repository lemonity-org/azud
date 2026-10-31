package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandSkipsConfig(t *testing.T) {
	root := &cobra.Command{Use: "azud"}
	completion := &cobra.Command{Use: "completion"}
	completion.AddCommand(&cobra.Command{Use: "bash"})
	env := &cobra.Command{Use: "env"}
	env.AddCommand(&cobra.Command{Use: "list"})
	agentGroup := &cobra.Command{Use: "agent"}
	agentGroup.AddCommand(&cobra.Command{Use: "init"})
	root.AddCommand(
		&cobra.Command{Use: "init"},
		&cobra.Command{Use: "version"},
		&cobra.Command{Use: "help"},
		completion,
		&cobra.Command{Use: cobra.ShellCompRequestCmd},
		&cobra.Command{Use: cobra.ShellCompNoDescRequestCmd},
		&cobra.Command{Use: "config"},
		&cobra.Command{Use: "deploy"},
		env,
		agentGroup,
	)

	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"init"}, want: true},
		{args: []string{"version"}, want: true},
		{args: []string{"help"}, want: true},
		{args: []string{"completion"}, want: true},
		{args: []string{"completion", "bash"}, want: true},
		{args: []string{cobra.ShellCompRequestCmd}, want: true},
		{args: []string{cobra.ShellCompNoDescRequestCmd}, want: true},
		{args: []string{"config"}, want: false},
		{args: []string{"deploy"}, want: false},
		{args: []string{"env", "list"}, want: false},
		{args: []string{"agent", "init"}, want: false},
	}
	for _, tt := range tests {
		cmd, _, err := root.Find(tt.args)
		if err != nil {
			t.Fatalf("Find(%v): %v", tt.args, err)
		}
		if got := commandSkipsConfig(cmd); got != tt.want {
			t.Errorf("commandSkipsConfig(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestCommandSkipsSecrets(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"config"}, want: true},
		{args: []string{"env", "list"}, want: false},
		{args: []string{"env", "push"}, want: false},
		{args: []string{"deploy"}, want: false},
	}
	for _, tt := range tests {
		cmd, _, err := rootCmd.Find(tt.args)
		if err != nil {
			t.Fatalf("Find(%v): %v", tt.args, err)
		}
		if got := commandSkipsSecrets(cmd); got != tt.want {
			t.Errorf("commandSkipsSecrets(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}

	parent := &cobra.Command{Use: "parent", Annotations: map[string]string{skipSecretsAnnotation: "true"}}
	child := &cobra.Command{Use: "child"}
	parent.AddCommand(child)
	if !commandSkipsSecrets(child) {
		t.Error("commandSkipsSecrets did not inherit the annotation from a parent")
	}
}
