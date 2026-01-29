package podman

import (
	"strings"
	"testing"
)

func TestBuildRunCommand_Basic(t *testing.T) {
	cfg := &ContainerConfig{
		Name:   "myapp",
		Image:  "myapp:latest",
		Detach: true,
	}

	cmd := cfg.BuildRunCommand()

	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("expected command to start with 'podman ', got: %s", cmd)
	}
	if !strings.Contains(cmd, "run") {
		t.Error("expected 'run' in command")
	}
	if !strings.Contains(cmd, "-d") {
		t.Error("expected '-d' flag for detach")
	}
	if !strings.Contains(cmd, "--name myapp") {
		t.Error("expected '--name myapp'")
	}
	if !strings.Contains(cmd, "myapp:latest") {
		t.Error("expected image 'myapp:latest'")
	}
}

func TestBuildRunCommand_NeverDocker(t *testing.T) {
	cfg := &ContainerConfig{
		Image: "nginx:latest",
	}

	cmd := cfg.BuildRunCommand()

	if strings.HasPrefix(cmd, "docker ") {
		t.Errorf("command must not start with 'docker', got: %s", cmd)
	}
	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("command must start with 'podman', got: %s", cmd)
	}
}

func TestBuildRunCommand_WithPorts(t *testing.T) {
	cfg := &ContainerConfig{
		Image: "nginx:latest",
		Ports: []string{"8080:80", "8443:443"},
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "-p 8080:80") {
		t.Error("expected '-p 8080:80'")
	}
	if !strings.Contains(cmd, "-p 8443:443") {
		t.Error("expected '-p 8443:443'")
	}
}

func TestBuildRunCommand_WithVolumes(t *testing.T) {
	cfg := &ContainerConfig{
		Image:   "nginx:latest",
		Volumes: []string{"/data:/app/data"},
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "-v /data:/app/data") {
		t.Error("expected '-v /data:/app/data'")
	}
}

func TestBuildRunCommand_WithNetwork(t *testing.T) {
	cfg := &ContainerConfig{
		Image:   "nginx:latest",
		Network: "azud",
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "--network azud") {
		t.Error("expected '--network azud'")
	}
}

func TestBuildRunCommand_WithRestart(t *testing.T) {
	cfg := &ContainerConfig{
		Image:   "nginx:latest",
		Restart: "always",
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "--restart always") {
		t.Error("expected '--restart always'")
	}
}

func TestBuildRunCommand_WithResources(t *testing.T) {
	cfg := &ContainerConfig{
		Image:  "nginx:latest",
		Memory: "512m",
		CPUs:   "0.5",
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "--memory 512m") {
		t.Error("expected '--memory 512m'")
	}
	if !strings.Contains(cmd, "--cpus 0.5") {
		t.Error("expected '--cpus 0.5'")
	}
}

func TestBuildRunCommand_WithRemove(t *testing.T) {
	cfg := &ContainerConfig{
		Image:  "nginx:latest",
		Remove: true,
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "--rm") {
		t.Error("expected '--rm' flag")
	}
}

func TestBuildRunCommand_WithCommand(t *testing.T) {
	cfg := &ContainerConfig{
		Image:   "ruby:latest",
		Command: []string{"rails", "server"},
	}

	cmd := cfg.BuildRunCommand()

	if !strings.HasSuffix(cmd, "ruby:latest rails server") {
		t.Errorf("expected command to end with 'ruby:latest rails server', got: %s", cmd)
	}
}

func TestBuildRunCommand_WithHealthcheck(t *testing.T) {
	cfg := &ContainerConfig{
		Image:          "myapp:latest",
		HealthCmd:      "curl -f http://localhost:3000/up",
		HealthInterval: "10s",
		HealthTimeout:  "5s",
		HealthRetries:  3,
	}

	cmd := cfg.BuildRunCommand()

	if !strings.Contains(cmd, "--health-cmd") {
		t.Error("expected '--health-cmd'")
	}
	if !strings.Contains(cmd, "--health-interval 10s") {
		t.Error("expected '--health-interval 10s'")
	}
	if !strings.Contains(cmd, "--health-timeout 5s") {
		t.Error("expected '--health-timeout 5s'")
	}
	if !strings.Contains(cmd, "--health-retries 3") {
		t.Error("expected '--health-retries 3'")
	}
}

func TestBuildExecCommand_Basic(t *testing.T) {
	cfg := &ExecConfig{
		Container: "myapp",
		Command:   []string{"ls", "-la"},
	}

	cmd := cfg.BuildExecCommand()

	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("expected command to start with 'podman ', got: %s", cmd)
	}
	if !strings.Contains(cmd, "exec") {
		t.Error("expected 'exec' in command")
	}
	if !strings.Contains(cmd, "myapp") {
		t.Error("expected container name 'myapp'")
	}
	if !strings.HasSuffix(cmd, "ls -la") {
		t.Errorf("expected command to end with 'ls -la', got: %s", cmd)
	}
}

func TestBuildExecCommand_NeverDocker(t *testing.T) {
	cfg := &ExecConfig{
		Container: "test",
		Command:   []string{"echo"},
	}

	cmd := cfg.BuildExecCommand()

	if strings.HasPrefix(cmd, "docker ") {
		t.Errorf("command must not start with 'docker', got: %s", cmd)
	}
}

func TestBuildExecCommand_Interactive(t *testing.T) {
	cfg := &ExecConfig{
		Container:   "myapp",
		Command:     []string{"/bin/sh"},
		Interactive: true,
		TTY:         true,
	}

	cmd := cfg.BuildExecCommand()

	if !strings.Contains(cmd, "-i") {
		t.Error("expected '-i' flag for interactive")
	}
	if !strings.Contains(cmd, "-t") {
		t.Error("expected '-t' flag for TTY")
	}
}

func TestBuildLogsCommand_Basic(t *testing.T) {
	cfg := &LogsConfig{
		Container: "myapp",
	}

	cmd := cfg.BuildLogsCommand()

	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("expected command to start with 'podman ', got: %s", cmd)
	}
	if !strings.Contains(cmd, "logs") {
		t.Error("expected 'logs' in command")
	}
}

func TestBuildLogsCommand_NeverDocker(t *testing.T) {
	cfg := &LogsConfig{
		Container: "test",
	}

	cmd := cfg.BuildLogsCommand()

	if strings.HasPrefix(cmd, "docker ") {
		t.Errorf("command must not start with 'docker', got: %s", cmd)
	}
}

func TestBuildLogsCommand_WithFollow(t *testing.T) {
	cfg := &LogsConfig{
		Container: "myapp",
		Follow:    true,
		Tail:      "100",
	}

	cmd := cfg.BuildLogsCommand()

	if !strings.Contains(cmd, "-f") {
		t.Error("expected '-f' flag for follow")
	}
	if !strings.Contains(cmd, "--tail 100") {
		t.Error("expected '--tail 100'")
	}
}
