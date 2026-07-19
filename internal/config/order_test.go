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
