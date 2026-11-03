package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SpecLabel is the label on an accessory or cron container that holds the hash of
// the spec it was created from. A container without it was created by an earlier
// Azud, so what it was created from is unknown.
const SpecLabel = "azud.spec"

// accessorySpec and cronSpec are what a container is created from. Their JSON is
// what gets hashed, so a field added later must be omitted when empty, or every
// container running today would count as changed. Hosts and boot_timeout do not
// change a container, so they are not here.
type accessorySpec struct {
	Image       string            `json:"image"`
	Port        string            `json:"port,omitempty"`
	Clear       map[string]string `json:"clear,omitempty"`
	Secret      []string          `json:"secret,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Files       []FileMapping     `json:"files,omitempty"`
	Directories []string          `json:"directories,omitempty"`
	Cmd         string            `json:"cmd,omitempty"`
	Options     map[string]string `json:"options,omitempty"`
}

type cronSpec struct {
	Image    string            `json:"image"`
	Schedule string            `json:"schedule"`
	Command  string            `json:"command"`
	LogPath  string            `json:"log_path,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	Lock     bool              `json:"lock,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Clear    map[string]string `json:"clear,omitempty"`
	Secret   []string          `json:"secret,omitempty"`
	Volumes  []string          `json:"volumes,omitempty"`
}

// AccessorySpec returns the hash of what the named accessory's container is created
// from. Secret names count and their values do not.
func (c *Config) AccessorySpec(name string) string {
	a := c.Accessories[name]
	return specHash(accessorySpec{
		Image:       a.Image,
		Port:        a.Port,
		Clear:       a.Env.Clear,
		Secret:      a.Env.Secret,
		Volumes:     a.Volumes,
		Files:       a.Files,
		Directories: a.Directories,
		Cmd:         a.Cmd,
		Options:     a.Options,
	})
}

// CronSpec returns the hash of what the named job's scheduled container is created
// from: the job, the application's image, and the environment and volumes it takes
// from the application. Secret names count and their values do not.
func (c *Config) CronSpec(name string) string {
	job := c.Cron[name]
	return specHash(cronSpec{
		Image:    c.Image,
		Schedule: job.Schedule,
		Command:  job.Command,
		LogPath:  job.LogPath,
		Timeout:  job.Timeout,
		Lock:     job.Lock,
		Env:      job.Env,
		Clear:    c.Env.Clear,
		Secret:   c.Env.Secret,
		Volumes:  c.Volumes,
	})
}

// specHash hashes v's JSON, which sorts map keys, so equal specs hash the same.
func specHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		// Strings, slices and maps of strings always marshal.
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
