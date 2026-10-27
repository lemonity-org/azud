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

// Control: Restart, WantedBy, and the network/volume names go through the same
// INI sanitizer as every other field, and the validators reject them. The
// checks use line prefixes: the sanitized value still contains the payload text
// on its own line.
func TestControl_QuadletServiceInstallAndNamesCannotInjectDirectives(t *testing.T) {
	injected := "always\n[Service]\nExecStartPre=/bin/sh -c 'touch /tmp/quadlet-latent-pwn'"
	unit := &quadlet.ContainerUnit{
		Image:    "alpine",
		Restart:  injected,
		WantedBy: injected,
	}
	out := quadlet.GenerateContainerFile(unit)
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
	if err := unit.Validate(); err == nil {
		t.Fatal("ContainerUnit.Validate accepted an injected Restart/WantedBy value")
	}

	netFile := quadlet.GenerateNetworkFile("azud\n[Network]\nDNS=false", true)
	if exactLineCount(netFile, "[Network]") != 1 || linePrefixCount(netFile, "DNS=") != 1 {
		t.Fatalf("GenerateNetworkFile name injection detected:\n%s", netFile)
	}
	if err := quadlet.ValidateNetworkName("azud\n[Network]"); err == nil {
		t.Fatal("ValidateNetworkName accepted an injected name")
	}

	volFile := quadlet.GenerateVolumeFile("data\n[Volume]\nDriver=evil")
	if exactLineCount(volFile, "[Volume]") != 1 || linePrefixCount(volFile, "Driver=") != 0 {
		t.Fatalf("GenerateVolumeFile name injection detected:\n%s", volFile)
	}
	if err := quadlet.ValidateVolumeName("data\n"); err == nil {
		t.Fatal("ValidateVolumeName accepted an injected name")
	}

	// A trailing backslash is a systemd line continuation. It must not let
	// Exec swallow the hardening flag on the following line.
	continued := quadlet.GenerateContainerFile(&quadlet.ContainerUnit{
		Image:      "alpine",
		Exec:       "sleep 1 \\",
		PodmanArgs: []string{"--read-only"},
	})
	if linePrefixCount(continued, "PodmanArgs=--read-only") != 1 {
		t.Fatalf("PodmanArgs directive was not preserved:\n%s", continued)
	}
	for _, line := range strings.Split(continued, "\n") {
		if strings.HasSuffix(line, "\\") {
			t.Fatalf("line continuation detected: %q", line)
		}
	}
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

func linePrefixCount(s, prefix string) int {
	count := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			count++
		}
	}
	return count
}
