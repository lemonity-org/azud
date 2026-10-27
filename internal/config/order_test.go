package config

import (
	"reflect"
	"testing"
)

func TestOperationalOrderingIsDeterministic(t *testing.T) {
	cfg := &Config{
		Servers: map[string]RoleConfig{
			"worker": {Hosts: []string{"worker-b", "shared"}},
			"web":    {Hosts: []string{"web-a", "shared"}},
		},
		Accessories: map[string]AccessoryConfig{
			"redis":    {Host: "redis-host"},
			"postgres": {Hosts: []string{"db-b", "db-a"}},
		},
		Cron: map[string]CronConfig{
			"weekly": {Host: "cron-b"},
			"daily":  {Host: "cron-a"},
		},
	}

	wantRoles := []string{"web", "worker"}
	wantHosts := []string{"web-a", "shared", "worker-b"}
	wantAccessories := []string{"postgres", "redis"}
	wantCron := []string{"daily", "weekly"}
	wantCronHosts := []string{"cron-a", "cron-b"}
	for i := 0; i < 100; i++ {
		if got := cfg.GetRoles(); !reflect.DeepEqual(got, wantRoles) {
			t.Fatalf("roles iteration %d = %v", i, got)
		}
		if got := cfg.GetAllHosts(); !reflect.DeepEqual(got, wantHosts) {
			t.Fatalf("hosts iteration %d = %v", i, got)
		}
		if got := cfg.GetAccessoryNames(); !reflect.DeepEqual(got, wantAccessories) {
			t.Fatalf("accessories iteration %d = %v", i, got)
		}
		if got := cfg.GetCronNames(); !reflect.DeepEqual(got, wantCron) {
			t.Fatalf("cron names iteration %d = %v", i, got)
		}
		if got := cfg.GetAllCronHosts(); !reflect.DeepEqual(got, wantCronHosts) {
			t.Fatalf("cron hosts iteration %d = %v", i, got)
		}
	}
}

func TestGetWorkloadHostsExcludesBuilderAndSSHProxy(t *testing.T) {
	cfg := &Config{
		Servers: map[string]RoleConfig{
			"web":    {Hosts: []string{"web-a", "shared"}},
			"worker": {Hosts: []string{"worker-a"}},
		},
		Accessories: map[string]AccessoryConfig{
			"db":    {Host: "db-host"},
			"redis": {Hosts: []string{"redis-host", "shared"}},
		},
		Cron: map[string]CronConfig{
			"default":  {},
			"explicit": {Host: "cron-host"},
		},
		Builder: BuilderConfig{Remote: RemoteBuilderConfig{Host: "builder-host"}},
		SSH:     SSHConfig{Proxy: SSHProxyConfig{Host: "bastion-host"}},
	}

	wantWorkload := []string{"web-a", "shared", "worker-a", "db-host", "redis-host", "cron-host"}
	if got := cfg.GetWorkloadHosts(); !reflect.DeepEqual(got, wantWorkload) {
		t.Fatalf("GetWorkloadHosts() = %v, want %v", got, wantWorkload)
	}

	wantSSH := append(append([]string{}, wantWorkload...), "builder-host", "bastion-host")
	if got := cfg.GetAllSSHHosts(); !reflect.DeepEqual(got, wantSSH) {
		t.Fatalf("GetAllSSHHosts() = %v, want %v", got, wantSSH)
	}
}

func TestGetWorkloadHostsKeepsBuilderThatAlsoRunsWorkload(t *testing.T) {
	cfg := &Config{
		Servers: map[string]RoleConfig{"web": {Hosts: []string{"shared"}}},
		Builder: BuilderConfig{Remote: RemoteBuilderConfig{Host: "shared"}},
		SSH:     SSHConfig{Proxy: SSHProxyConfig{Host: "bastion-host"}},
	}

	if got, want := cfg.GetWorkloadHosts(), []string{"shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GetWorkloadHosts() = %v, want %v", got, want)
	}
}
