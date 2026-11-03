package config

import "testing"

func specConfig() *Config {
	return &Config{
		Image:   "registry.test/shop@sha256:aa11",
		Env:     EnvConfig{Clear: map[string]string{"A": "1", "B": "2"}, Secret: []string{"APP_SECRET"}},
		Volumes: []string{"/srv/uploads:/uploads"},
		Accessories: map[string]AccessoryConfig{
			"db": {Image: "postgres:16", Host: "10.0.0.1", Env: EnvConfig{Secret: []string{"DB_PASSWORD"}}},
		},
		Cron: map[string]CronConfig{
			"backup": {Schedule: "0 2 * * *", Command: "bin/backup", Lock: true},
		},
	}
}

// These are what containers running today carry. A change here makes every one of
// them read as changed, and makes cron boot recreate them.
func TestSpecHashesAreStable(t *testing.T) {
	cfg := specConfig()
	if got, want := cfg.AccessorySpec("db"), "c950825f682a14ae4379938476b430b887c1f8be5b6cf27838cb4f4199d89e33"; got != want {
		t.Errorf("AccessorySpec = %s, want %s", got, want)
	}
	if got, want := cfg.CronSpec("backup"), "7924768d0ad2a8ab30c6ee18c86938a7a9e5924eb9ae366dc4c21b809a700155"; got != want {
		t.Errorf("CronSpec = %s, want %s", got, want)
	}
}

func TestAccessorySpecChangesWithWhatTheContainerIsCreatedFrom(t *testing.T) {
	changes := map[string]func(*AccessoryConfig){
		"image":       func(a *AccessoryConfig) { a.Image = "postgres:17" },
		"port":        func(a *AccessoryConfig) { a.Port = "5432:5432" },
		"clear env":   func(a *AccessoryConfig) { a.Env.Clear = map[string]string{"X": "1"} },
		"secret name": func(a *AccessoryConfig) { a.Env.Secret = []string{"OTHER"} },
		"volumes":     func(a *AccessoryConfig) { a.Volumes = []string{"/a:/b"} },
		"directories": func(a *AccessoryConfig) { a.Directories = []string{"/a"} },
		"cmd":         func(a *AccessoryConfig) { a.Cmd = "postgres -c x=y" },
		"options":     func(a *AccessoryConfig) { a.Options = map[string]string{"memory": "1G"} },
		"files":       func(a *AccessoryConfig) { a.Files = []FileMapping{{Local: "a", Remote: "/b"}} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cfg := specConfig()
			before := cfg.AccessorySpec("db")
			a := cfg.Accessories["db"]
			change(&a)
			cfg.Accessories["db"] = a
			if cfg.AccessorySpec("db") == before {
				t.Error("the hash did not change")
			}
		})
	}
}

func TestAccessorySpecIgnoresWhatDoesNotChangeTheContainer(t *testing.T) {
	cfg := specConfig()
	before := cfg.AccessorySpec("db")
	a := cfg.Accessories["db"]
	a.Host, a.Hosts = "10.0.0.2", []string{"10.0.0.3"}
	cfg.Accessories["db"] = a
	cfg.Image = "registry.test/shop@sha256:bb22"
	if cfg.AccessorySpec("db") != before {
		t.Error("placement or the application's image changed an accessory's hash")
	}
}

func TestCronSpecChangesWithWhatTheContainerIsCreatedFrom(t *testing.T) {
	changes := map[string]func(*Config){
		"image":       func(c *Config) { c.Image = "registry.test/shop@sha256:bb22" },
		"app env":     func(c *Config) { c.Env.Clear = map[string]string{"A": "changed"} },
		"secret name": func(c *Config) { c.Env.Secret = []string{"OTHER"} },
		"volumes":     func(c *Config) { c.Volumes = nil },
		"schedule":    func(c *Config) { j := c.Cron["backup"]; j.Schedule = "0 3 * * *"; c.Cron["backup"] = j },
		"command":     func(c *Config) { j := c.Cron["backup"]; j.Command = "bin/other"; c.Cron["backup"] = j },
		"lock":        func(c *Config) { j := c.Cron["backup"]; j.Lock = false; c.Cron["backup"] = j },
		"timeout":     func(c *Config) { j := c.Cron["backup"]; j.Timeout = "1h"; c.Cron["backup"] = j },
		"log path":    func(c *Config) { j := c.Cron["backup"]; j.LogPath = "/a"; c.Cron["backup"] = j },
		"job env":     func(c *Config) { j := c.Cron["backup"]; j.Env = map[string]string{"X": "1"}; c.Cron["backup"] = j },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cfg := specConfig()
			before := cfg.CronSpec("backup")
			change(cfg)
			if cfg.CronSpec("backup") == before {
				t.Error("the hash did not change")
			}
		})
	}
}

func TestSpecHashDoesNotDependOnMapOrder(t *testing.T) {
	first := specConfig().CronSpec("backup")
	for range 20 {
		if got := specConfig().CronSpec("backup"); got != first {
			t.Fatalf("hash changed between runs: %s, %s", first, got)
		}
	}
}
