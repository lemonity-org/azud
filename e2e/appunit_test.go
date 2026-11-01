//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/quadlet"
)

// TestDeployIsRefusedWhileAnAppUnitIsActive guards against a silent revert. A unit from
// `azud systemd enable` has Restart=always and a pinned image, and Quadlet runs it with
// --replace. A rolling swap stops the old container, so systemd restarts the unit, which
// replaces the new release with the pinned image while the agent records the new one.
func TestDeployIsRefusedWhileAnAppUnitIsActive(t *testing.T) {
	requireSystemd(t)
	s := newService(t, "appunit")
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	unit := s.roleContainer()
	s.installAppUnit(unit, s.repo+"@"+first)
	t.Cleanup(func() { s.removeAppUnit(unit) })
	s.waitServes("v1")
	running := s.inspect("{{.Id}}")

	second := s.release("v2")
	out, err := s.agentOnce()
	if err == nil || !strings.Contains(out, unit+".service") || !strings.Contains(out, "stop it before deploying") {
		t.Fatalf("deploy with the unit active: err=%v, want the unit named in a refusal\n%s", err, out)
	}
	s.waitServes("v1")
	if after := s.inspect("{{.Id}}"); after != running {
		t.Errorf("a refused deploy replaced the container: %s to %s", running, after)
	}
	st := s.agentState()
	if st.DeployedDigest != first || st.FailedDigest != second {
		t.Errorf("state = %+v, want deployed %s and failed %s", st, first, second)
	}

	// The way through that the refusal names: stop the unit, then deploy.
	s.systemctl("stop", unit+".service")
	if out, err := s.agentOnce("--retry-failed"); err != nil {
		t.Fatalf("retry with the unit stopped: %v\n%s", err, out)
	}
	s.waitServes("v2")

	// Long enough for a unit restart to have undone it, as it did before the guard.
	time.Sleep(30 * time.Second)
	if got := s.inspect("{{.ImageName}}"); got != s.repo+"@"+second {
		t.Errorf("container runs %q, want the deployed %s", got, second)
	}
	s.waitServes("v2")
}

// requireSystemd asks whether the service manager answers, not whether it reports
// "running": a CI runner can stay in "starting" indefinitely while its manager works.
func requireSystemd(t *testing.T) {
	t.Helper()
	out, err := run("systemctl", systemctlArgs("show", "--property=Version")...)
	if err == nil && strings.HasPrefix(out, "Version=") {
		return
	}
	if os.Getenv("AZUD_E2E_REQUIRE_SYSTEMD") == "1" {
		t.Fatalf("systemd is required here, but the manager did not answer: %v\n%s", err, out)
	}
	t.Skipf("no usable systemd for this user: %v", err)
}

func systemctlArgs(args ...string) []string {
	if h.root {
		return args
	}
	return append([]string{"--user"}, args...)
}

func journalArgs(args ...string) []string {
	if h.root {
		return args
	}
	return append([]string{"--user"}, args...)
}

func (s *service) quadletDir() string {
	if h.root {
		return "/etc/containers/systemd"
	}
	return filepath.Join(h.home, ".config/containers/systemd")
}

// installAppUnit installs what `azud systemd enable` installs for a role, then starts
// it. That command dials SSH on v1, so the test writes the same units through the same
// generator, and starting the unit takes over the stable name through --replace.
func (s *service) installAppUnit(name, image string) {
	s.t.Helper()
	dir := s.quadletDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	files := map[string]string{
		"azud.network": quadlet.GenerateNetworkFile("azud", true),
		name + ".container": quadlet.GenerateContainerFile(&quadlet.ContainerUnit{
			Description:   fmt.Sprintf("Azud service %s (web role)", s.name),
			Image:         image,
			ContainerName: name,
			Network:       []string{"azud.network"},
			PublishPort:   s.publishedPorts(name),
			Label:         map[string]string{"azud.service": s.name, "azud.role": "web"},
			Restart:       "always",
			WantedBy:      "default.target",
		}),
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	s.systemctl("daemon-reload")
	if out, err := run("systemctl", systemctlArgs("start", name+".service")...); err != nil {
		status, _ := run("systemctl", systemctlArgs("status", "--no-pager", "-l", name+".service")...)
		journal, _ := run("journalctl", journalArgs("--no-pager", "-n", "40", "-u", name+".service")...)
		s.t.Fatalf("start %s: %v\n%s\nstatus:\n%s\njournal:\n%s", name, err, out, status, journal)
	}
}

// publishedPorts returns the running container's port mappings in Quadlet's form. With a
// rootful proxy, the proxy reaches a rootless app through a published loopback port, so
// the unit must keep it, as `azud systemd enable` does.
func (s *service) publishedPorts(name string) []string {
	s.t.Helper()
	out, err := podman("port", name)
	if err != nil {
		s.t.Fatalf("podman port %s: %v\n%s", name, err, out)
	}
	var ports []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// 8080/tcp -> 127.0.0.1:34567
		inner, outer, ok := strings.Cut(line, " -> ")
		if !ok {
			continue
		}
		port, _, _ := strings.Cut(inner, "/")
		ports = append(ports, outer+":"+port)
	}
	return ports
}

func (s *service) removeAppUnit(name string) {
	_, _ = run("systemctl", systemctlArgs("stop", name+".service")...)
	_ = os.Remove(filepath.Join(s.quadletDir(), name+".container"))
	_ = os.Remove(filepath.Join(s.quadletDir(), "azud.network"))
	_, _ = run("systemctl", systemctlArgs("daemon-reload")...)
}

func (s *service) systemctl(args ...string) {
	s.t.Helper()
	if out, err := run("systemctl", systemctlArgs(args...)...); err != nil {
		s.t.Fatalf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
