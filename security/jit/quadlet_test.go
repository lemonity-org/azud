//go:build security

package jit

import (
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/quadlet"
)

// payload is a worst-case INI value: newline-delimited directives, quotes,
// percent specifiers, and control characters.
const quadletPayload = "safe\"value\"\n[Service]\nExecStartPre=/bin/sh -c 'touch /tmp/quadlet-pwn'\n%h%t\r\x01\x7f"

func TestControl_QuadletContainerFieldsCannotInjectDirectives(t *testing.T) {
	unit := &quadlet.ContainerUnit{
		Description:     "desc" + quadletPayload,
		After:           []string{"after" + quadletPayload},
		Requires:        []string{"requires" + quadletPayload},
		Image:           "image" + quadletPayload,
		ContainerName:   "name" + quadletPayload,
		Environment:     map[string]string{"KEY" + quadletPayload: "VALUE" + quadletPayload},
		EnvironmentFile: []string{"file" + quadletPayload},
		PublishPort:     []string{"port" + quadletPayload},
		Volume:          []string{"volume" + quadletPayload},
		Network:         []string{"network" + quadletPayload},
		Label:           map[string]string{"label" + quadletPayload: "value" + quadletPayload},
		HealthCmd:       "health" + quadletPayload,
		HealthInterval:  "interval" + quadletPayload,
		Exec:            "exec" + quadletPayload,
		PodmanArgs:      []string{"--arg=" + quadletPayload},
	}
	out := quadlet.GenerateContainerFile(unit)

	// Every value must stay on one logical line: no injected section and no
	// injected ExecStartPre directive.
	for _, section := range []string{"[Unit]", "[Container]", "[Service]", "[Install]"} {
		if exactLineCount(out, section) != 1 {
			t.Fatalf("section injection detected for %s:\n%s", section, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ExecStartPre=") {
			t.Fatalf("directive injection detected: %q", line)
		}
	}
	if !strings.Contains(out, "%%h%%t") {
		t.Fatalf("percent specifiers in Environment were not escaped:\n%s", out)
	}
}

// Finding: Restart, WantedBy, and the network/volume generators bypass
// sanitizeINIValue. Current callers use constants, but the exported API is
// unsafe for any future config-driven caller.
func TestFinding_QuadletUnsanitizedFieldsInjectDirectives(t *testing.T) {
	injected := "always\n[Service]\nExecStartPre=/bin/sh -c 'touch /tmp/quadlet-latent-pwn'"
	unit := &quadlet.ContainerUnit{
		Image:    "alpine",
		Restart:  injected,
		WantedBy: injected,
	}
	out := quadlet.GenerateContainerFile(unit)
	if !strings.Contains(out, "ExecStartPre=") {
		t.Fatalf("expected unsanitized Restart/WantedBy injection, got:\n%s", out)
	}
	t.Logf("VULNERABLE (latent): ContainerUnit.Restart and WantedBy are written without sanitizeINIValue:\n%s", out)

	netFile := quadlet.GenerateNetworkFile("azud\n[Network]\nDNS=false", true)
	if strings.Count(netFile, "DNS=") != 2 {
		t.Fatalf("expected GenerateNetworkFile name injection, got:\n%s", netFile)
	}
	volFile := quadlet.GenerateVolumeFile("data\n[Volume]\nDriver=evil")
	if strings.Count(volFile, "[Volume]") != 2 {
		t.Fatalf("expected GenerateVolumeFile name injection, got:\n%s", volFile)
	}
	t.Logf("VULNERABLE (latent): GenerateNetworkFile/GenerateVolumeFile names are not sanitized")
}

func exactLineCount(s, want string) int {
	count := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == want {
			count++
		}
	}
	return count
}
