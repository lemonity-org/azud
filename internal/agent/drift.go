package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/podman"
)

// DriftState is how an accessory or cron job differs from what the release specifies.
type DriftState string

const (
	// DriftChanged means the container was created from another spec.
	DriftChanged DriftState = "changed"
	// DriftUnknown means the container has no spec label because an earlier Azud
	// created it, so it may or may not match.
	DriftUnknown DriftState = "unknown"
	// DriftMissing means the release specifies it and no container exists.
	DriftMissing DriftState = "missing"
	// DriftExtra means a container exists that the release does not place here.
	DriftExtra DriftState = "extra"
)

// Kinds of Drift.
const (
	DriftAccessory = "accessory"
	DriftCron      = "cron"
)

// Drift is one difference, with the command that applies the release. A deploy leaves
// accessories and cron jobs as they are, so applying them is the operator's choice.
type Drift struct {
	Kind      string
	Name      string
	Container string
	State     DriftState
	Command   string
}

// DetectDrift compares containers, this service's on host, with what cfg specifies
// for host. Pass the effective configuration, so cfg is the release. It returns only
// differences, accessories first, each kind sorted by name.
func DetectDrift(cfg *config.Config, host string, containers []podman.Container) []Drift {
	accessories := make(map[string]podman.Container)
	crons := make(map[string]podman.Container)
	for _, c := range containers {
		if c.Labels["azud.service"] != cfg.Service {
			continue
		}
		if name := c.Labels["azud.accessory"]; name != "" {
			accessories[name] = c
		}
		// A manual `cron run` container goes away by itself.
		if name := c.Labels["azud.cron"]; name != "" && c.Labels["azud.cron.run"] == "" {
			crons[name] = c
		}
	}

	var drift []Drift
	for _, name := range cfg.GetAccessoryNames() {
		accessory := cfg.Accessories[name]
		if accessory.Host != host && !slices.Contains(accessory.Hosts, host) {
			continue
		}
		spec := cfg.AccessorySpec(name)
		container, exists := accessories[name]
		delete(accessories, name)
		if state, differs := specDrift(container, exists, spec); differs {
			command := "azud accessory boot " + name
			if exists {
				command = "azud accessory remove " + name + " && " + command
			}
			drift = append(drift, Drift{Kind: DriftAccessory, Name: name, Container: orName(container, cfg.Service+"-"+name), State: state, Command: command})
		}
	}
	drift = append(drift, extraDrift(DriftAccessory, accessories)...)

	for _, name := range cfg.GetCronNames() {
		if !slices.Contains(cfg.GetCronHosts(name), host) {
			continue
		}
		spec := cfg.CronSpec(name)
		container, exists := crons[name]
		delete(crons, name)
		if state, differs := specDrift(container, exists, spec); differs {
			drift = append(drift, Drift{Kind: DriftCron, Name: name, Container: orName(container, fmt.Sprintf("%s-cron-%s", cfg.Service, name)), State: state, Command: "azud cron boot " + name})
		}
	}
	return append(drift, extraDrift(DriftCron, crons)...)
}

func specDrift(container podman.Container, exists bool, spec string) (DriftState, bool) {
	if !exists {
		return DriftMissing, true
	}
	switch container.Labels[config.SpecLabel] {
	case spec:
		return "", false
	case "":
		return DriftUnknown, true
	default:
		return DriftChanged, true
	}
}

// extraDrift reports containers the release no longer has. No azud command takes a
// name the release does not have, so applying it goes through Podman.
func extraDrift(kind string, containers map[string]podman.Container) []Drift {
	var drift []Drift
	for name, container := range containers {
		drift = append(drift, Drift{Kind: kind, Name: name, Container: container.Name, State: DriftExtra, Command: "podman rm -f " + container.Name})
	}
	slices.SortFunc(drift, func(a, b Drift) int { return strings.Compare(a.Name, b.Name) })
	return drift
}

func orName(container podman.Container, fallback string) string {
	if container.Name != "" {
		return container.Name
	}
	return fallback
}
