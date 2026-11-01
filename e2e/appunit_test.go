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

// TestRollingDeployWithAppUnit settles the question behind removing app units in v2.
// A unit from `azud systemd enable` has Restart=always and a pinned image, and Quadlet
// runs it with --replace. A rolling swap renames the new container to the stable name
// and stops the old one, so systemd restarts the unit, which replaces the new release
// with the pinned old image. First run on v1.6.0 and Podman 5.7, rootless: the deploy
// reports success, the proxy then times out for tens of seconds, and the old release
// comes back while the agent still records the new one.
//
// It reports what happened instead of asserting it, because on v1 the behaviour is what
// is in question and no outcome should turn CI red. It fails only if the container
// runs something that is neither release.
func TestRollingDeployWithAppUnit(t *testing.T) {
	requireSystemd(t)
	s := newService(t, "appunit")
	first := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	unit := s.roleContainer()
	s.installAppUnit(unit, s.repo+"@"+first)
	t.Cleanup(func() { s.removeAppUnit(unit) })
	s.waitServes("v1")

	second := s.release("v2")
	out, deployErr := s.agentOnce()
	t.Logf("deploy with the unit active: err=%v\n%s", deployErr, out)

	// Restart=always acts after RestartSec, so watch long enough to see it settle.
	var last string
	var final observation
	for i := 0; i < 16; i++ {
		final = s.observeRole(unit)
		if line := final.String(); line != last {
			t.Logf("t+%ds: %s", i*5, line)
			last = line
		}
		time.Sleep(5 * time.Second)
	}
	status, _ := run("systemctl", systemctlArgs("status", "--no-pager", "-l", unit+".service")...)
	containers, _ := podman("ps", "-a", "--filter", "label=azud.service="+s.name, "--format", "{{.Names}} {{.Image}} {{.Status}}")
	t.Logf("unit status:\n%s\ncontainers:\n%s", status, containers)

	switch {
	case final.image == "":
		t.Logf("OBSERVATION app-unit-race: gone. No container holds the stable name; the agent records %s and the proxy answers %q", s.agentState().DeployedDigest, final.serves)
	case final.image == s.repo+"@"+first:
		t.Logf("OBSERVATION app-unit-race: reverted. The stable container runs the unit's pinned %s, not the deployed %s; the proxy answers %q; the agent records %s", first, second, final.serves, s.agentState().DeployedDigest)
	case final.image == s.repo+"@"+second:
		t.Logf("OBSERVATION app-unit-race: kept. The stable container stays on the deployed %s; the proxy answers %q", second, final.serves)
	default:
		t.Fatalf("the stable container runs %q, which is neither release", final.image)
	}
}

// observation is one look at a role during the race test.
type observation struct {
	image, unit, serves string
}

func (o observation) String() string {
	return fmt.Sprintf("container=%q unit=%s proxy=%q", o.image, o.unit, o.serves)
}

func (s *service) observeRole(unit string) observation {
	image, err := podman("container", "inspect", "--format", "{{.ImageName}}", s.roleContainer())
	if err != nil {
		image = ""
	}
	active, _ := run("systemctl", systemctlArgs("is-active", unit+".service")...)
	body, serveErr := s.serves()
	if serveErr != nil {
		body = "error: " + serveErr.Error()
	}
	return observation{image: strings.TrimSpace(image), unit: strings.TrimSpace(active), serves: body}
}

func requireSystemd(t *testing.T) {
	t.Helper()
	out, _ := run("systemctl", systemctlArgs("is-system-running")...)
	state := strings.TrimSpace(out)
	if state == "running" || state == "degraded" {
		return
	}
	if os.Getenv("AZUD_E2E_REQUIRE_SYSTEMD") == "1" {
		t.Fatalf("systemd is required here, but its state is %q", state)
	}
	t.Skipf("no usable systemd for this user: %q", state)
}

func systemctlArgs(args ...string) []string {
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
	s.systemctl("start", name+".service")
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
