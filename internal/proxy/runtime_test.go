package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNewCaddyContainerConfigIsHardenedAndDoesNotPublishAdmin(t *testing.T) {
	config := NewCaddyContainerConfig(8080, 8443, false)
	if !reflect.DeepEqual(config.Ports, []string{"8080:80", "8443:443"}) {
		t.Fatalf("ports = %v", config.Ports)
	}
	for _, port := range config.Ports {
		if strings.Contains(port, "2019") {
			t.Fatalf("admin port is published: %v", config.Ports)
		}
	}
	if config.Env["CADDY_ADMIN"] != CaddyAdminListen || config.Network != "azud" {
		t.Fatalf("admin/network = %v/%q", config.Env, config.Network)
	}
	if config.User != "1000:1000" || !config.ReadOnly || !config.DisableReadOnlyTmpfs || !config.NoNewPrivileges {
		t.Fatalf("user/read-only hardening is incomplete: %#v", config)
	}
	if config.Entrypoint != CaddyRuntimeEntrypoint {
		t.Fatalf("entrypoint = %q, want %q", config.Entrypoint, CaddyRuntimeEntrypoint)
	}
	if !reflect.DeepEqual(config.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(config.CapAdd, []string{"NET_BIND_SERVICE"}) {
		t.Fatalf("capabilities = drop %v add %v", config.CapDrop, config.CapAdd)
	}
	if config.Memory != CaddyMemoryLimit || config.MemorySwap != CaddyMemorySwapLimit || config.CPUs != CaddyCPULimit || config.PidsLimit != CaddyPidsLimit {
		t.Fatalf("resource limits are incomplete: %#v", config)
	}
	if !strings.Contains(strings.Join(config.Command, " "), CaddyRecoveryConfigFile) || strings.Contains(strings.Join(config.Command, " "), "--watch") {
		t.Fatalf("startup command = %v", config.Command)
	}
	for _, volume := range config.Volumes {
		if !strings.HasSuffix(volume, ":U") {
			t.Fatalf("named volume does not migrate ownership for the non-root user: %q", volume)
		}
	}
}

func TestCaddyRecoveryWriterIsNetworklessAndLeastPrivilege(t *testing.T) {
	config := newCaddyRecoveryWriterConfig()
	if config.Image != CaddyImage || config.Entrypoint != CaddyRuntimeEntrypoint {
		t.Fatalf("writer image/entrypoint = %q/%q", config.Image, config.Entrypoint)
	}
	if config.Network != "none" || len(config.Ports) != 0 || len(config.Env) != 0 {
		t.Fatalf("writer has external connectivity: network=%q ports=%v env=%v", config.Network, config.Ports, config.Env)
	}
	if config.User != CaddyRuntimeUser+":"+CaddyRuntimeGroup || !config.ReadOnly || !config.DisableReadOnlyTmpfs || !config.NoNewPrivileges || !config.Remove {
		t.Fatalf("writer isolation is incomplete: %#v", config)
	}
	if !reflect.DeepEqual(config.CapDrop, []string{"ALL"}) || len(config.CapAdd) != 0 {
		t.Fatalf("writer capabilities = drop %v add %v", config.CapDrop, config.CapAdd)
	}
	if !reflect.DeepEqual(config.Volumes, []string{"caddy_config:/config:U"}) {
		t.Fatalf("writer volumes = %v", config.Volumes)
	}
	command := strings.Join(config.Command, " ")
	if !strings.Contains(command, "mktemp /config/caddy/") || !strings.Contains(command, "mv -f") || !strings.Contains(command, CaddyRecoveryConfigFile) {
		t.Fatalf("writer does not stage atomically: %q", command)
	}
}

func TestInspectCaddyRuntimeAcceptsExactSpecAndFindsSecurityDrift(t *testing.T) {
	var inspect caddyInspect
	inspect.ImageName = CaddyImage
	inspect.Config.Image = CaddyImage
	inspect.Config.User = "1000:1000"
	inspect.Config.Env = []string{"CADDY_ADMIN=" + CaddyAdminListen}
	inspect.Config.Labels = map[string]string{caddyRuntimeRevisionLabel: CaddyRuntimeRevision}
	inspect.Config.Cmd = append([]string(nil), caddyCommand...)
	inspect.Config.Entrypoint = inspectArgv{CaddyRuntimeEntrypoint}
	inspect.Config.StopTimeout = CaddyStopTimeout
	inspect.Mounts = []caddyMount{
		{Type: "volume", Name: "caddy_data", Destination: "/data", RW: true},
		{Type: "volume", Name: "caddy_config", Destination: "/config", RW: true},
	}
	inspect.HostConfig.NetworkMode = "azud"
	inspect.HostConfig.ReadonlyRootfs = true
	inspect.HostConfig.CapDrop = []string{"CAP_ALL"}
	inspect.HostConfig.CapAdd = []string{"CAP_NET_BIND_SERVICE"}
	inspect.HostConfig.SecurityOpt = []string{"no-new-privileges=true"}
	inspect.HostConfig.PidsLimit = CaddyPidsLimit
	inspect.HostConfig.Memory = caddyMemoryLimitBytes
	inspect.HostConfig.MemorySwap = caddyMemoryLimitBytes
	inspect.HostConfig.NanoCPUs = caddyCPULimitNanoCPUs
	inspect.HostConfig.ShmSize = caddyShmSizeBytes
	inspect.HostConfig.Ulimits = []caddyUlimit{{Name: "RLIMIT_NOFILE", Soft: CaddyNofileLimit, Hard: CaddyNofileLimit}}
	inspect.HostConfig.Tmpfs = map[string]string{
		"/tmp": "rw,nodev,nosuid,noexec,size=16m,mode=1777",
		"/run": "rw,nodev,nosuid,noexec,size=8m,mode=755",
	}
	inspect.HostConfig.PortBindings = map[string][]caddyPortBinding{
		"80/tcp":   {{HostPort: "8080"}},
		"443/tcp":  {{HostPort: "8443"}},
		"2019/tcp": nil,
	}
	inspect.NetworkSettings.Ports = map[string][]caddyPortBinding{
		"80/tcp":   {{HostPort: "8080"}},
		"443/tcp":  {{HostPort: "8443"}},
		"2019/tcp": nil,
	}

	raw, err := json.Marshal([]caddyInspect{inspect})
	if err != nil {
		t.Fatal(err)
	}
	if drift := inspectCaddyRuntime(string(raw), false, 8080, 8443); len(drift) != 0 {
		t.Fatalf("exact runtime reported drift: %v", drift)
	}

	inspect.Config.Env = []string{"CADDY_ADMIN=0.0.0.0:2019"}
	inspect.HostConfig.ReadonlyRootfs = false
	inspect.HostConfig.PortBindings["2019/tcp"] = []caddyPortBinding{{HostIP: "127.0.0.1", HostPort: "2019"}}
	inspect.Mounts = append(inspect.Mounts, caddyMount{Type: "bind", Destination: "/host", RW: true})
	inspect.HostConfig.Ulimits[0].Hard = CaddyNofileLimit + 1
	inspect.Config.StopTimeout = CaddyStopTimeout - 1
	raw, _ = json.Marshal([]caddyInspect{inspect})
	drift := strings.Join(inspectCaddyRuntime(string(raw), false, 8080, 8443), "\n")
	for _, want := range []string{
		"CADDY_ADMIN",
		"root filesystem is writable",
		"published port bindings",
		"mount set",
		"ulimit set",
		"stop timeout",
	} {
		if !strings.Contains(drift, want) {
			t.Fatalf("drift %q missing %q", drift, want)
		}
	}
}

func TestInspectArgvAcceptsPodmanStringAndArraySchemas(t *testing.T) {
	for _, raw := range []string{`"/bin/sh"`, `["/bin/sh"]`} {
		var got inspectArgv
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if !reflect.DeepEqual([]string(got), []string{"/bin/sh"}) {
			t.Fatalf("unmarshal %s = %v", raw, got)
		}
	}
}

func TestActivePortBindingsRequireExactHostPortsAndWildcardIngress(t *testing.T) {
	expected := map[string]int{"80/tcp": 8080, "443/tcp": 8443}
	valid := func() map[string][]caddyPortBinding {
		return map[string][]caddyPortBinding{
			"80/tcp":  {{HostIP: "", HostPort: "8080"}},
			"443/tcp": {{HostIP: "0.0.0.0", HostPort: "8443"}},
		}
	}
	if !activePortBindingsEqual(valid(), expected) {
		t.Fatal("exact public HTTP/HTTPS bindings were rejected")
	}

	for _, tt := range []struct {
		name   string
		mutate func(map[string][]caddyPortBinding)
	}{
		{name: "wrong host port", mutate: func(got map[string][]caddyPortBinding) {
			got["80/tcp"] = []caddyPortBinding{{HostPort: "18080"}}
		}},
		{name: "loopback-only ingress", mutate: func(got map[string][]caddyPortBinding) {
			got["443/tcp"] = []caddyPortBinding{{HostIP: "127.0.0.1", HostPort: "8443"}}
		}},
		{name: "duplicate binding", mutate: func(got map[string][]caddyPortBinding) {
			got["80/tcp"] = append(got["80/tcp"], caddyPortBinding{HostIP: "::", HostPort: "8080"})
		}},
		{name: "extra port", mutate: func(got map[string][]caddyPortBinding) {
			got["9000/tcp"] = []caddyPortBinding{{HostPort: "9000"}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := valid()
			tt.mutate(got)
			if activePortBindingsEqual(got, expected) {
				t.Fatalf("drifted bindings accepted: %v", got)
			}
		})
	}
}
