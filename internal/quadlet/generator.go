package quadlet

import (
	"fmt"
	"sort"
	"strings"
)

// ContainerUnit holds configuration for generating a .container quadlet file
type ContainerUnit struct {
	Description     string
	After           []string
	Requires        []string
	Image           string
	ContainerName   string
	Environment     map[string]string
	EnvironmentFile []string
	PublishPort     []string
	Volume          []string
	Network         []string
	Label           map[string]string
	HealthCmd       string
	HealthInterval  string
	Entrypoint      string
	Exec            string
	PodmanArgs      []string
	User            string
	Group           string
	ReadOnly        bool
	ReadOnlyTmpfs   *bool
	DropCapability  []string
	AddCapability   []string
	NoNewPrivileges bool
	Tmpfs           []string
	Memory          string
	PidsLimit       int
	ShmSize         string
	Ulimit          []string
	Restart         string // systemd restart policy: always, on-failure
	TimeoutStopSec  int
	WantedBy        string
}

func quoteSystemdWord(s string, escapePercent bool) string {
	s = sanitizeINIValue(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	if escapePercent {
		s = strings.ReplaceAll(s, "%", "%%")
	}
	return `"` + s + `"`
}

// sanitizeINIValue removes newlines and control characters from a string
// to prevent injection of additional systemd directives via crafted values.
func sanitizeINIValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') || r == 0x7F {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// GenerateContainerFile generates a .container quadlet INI file from the unit configuration
func GenerateContainerFile(unit *ContainerUnit) string {
	var sb strings.Builder

	// [Unit] section
	sb.WriteString("[Unit]\n")
	if unit.Description != "" {
		_, _ = fmt.Fprintf(&sb, "Description=%s\n", sanitizeINIValue(unit.Description))
	}
	for _, after := range unit.After {
		_, _ = fmt.Fprintf(&sb, "After=%s\n", sanitizeINIValue(after))
	}
	for _, req := range unit.Requires {
		_, _ = fmt.Fprintf(&sb, "Requires=%s\n", sanitizeINIValue(req))
	}
	sb.WriteString("\n")

	// [Container] section
	sb.WriteString("[Container]\n")
	if unit.Image != "" {
		_, _ = fmt.Fprintf(&sb, "Image=%s\n", sanitizeINIValue(unit.Image))
	}
	if unit.ContainerName != "" {
		_, _ = fmt.Fprintf(&sb, "ContainerName=%s\n", sanitizeINIValue(unit.ContainerName))
	}
	envKeys := make([]string, 0, len(unit.Environment))
	for key := range unit.Environment {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		assignment := sanitizeINIValue(key) + "=" + unit.Environment[key]
		_, _ = fmt.Fprintf(&sb, "Environment=%s\n", quoteSystemdWord(assignment, true))
	}
	for _, file := range unit.EnvironmentFile {
		_, _ = fmt.Fprintf(&sb, "EnvironmentFile=%s\n", quoteSystemdWord(file, false))
	}
	for _, port := range unit.PublishPort {
		_, _ = fmt.Fprintf(&sb, "PublishPort=%s\n", sanitizeINIValue(port))
	}
	for _, vol := range unit.Volume {
		_, _ = fmt.Fprintf(&sb, "Volume=%s\n", sanitizeINIValue(vol))
	}
	for _, net := range unit.Network {
		_, _ = fmt.Fprintf(&sb, "Network=%s\n", sanitizeINIValue(net))
	}
	labelKeys := make([]string, 0, len(unit.Label))
	for key := range unit.Label {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		assignment := sanitizeINIValue(key) + "=" + unit.Label[key]
		_, _ = fmt.Fprintf(&sb, "Label=%s\n", quoteSystemdWord(assignment, true))
	}
	if unit.HealthCmd != "" {
		_, _ = fmt.Fprintf(&sb, "HealthCmd=%s\n", sanitizeINIValue(unit.HealthCmd))
	}
	if unit.HealthInterval != "" {
		_, _ = fmt.Fprintf(&sb, "HealthInterval=%s\n", sanitizeINIValue(unit.HealthInterval))
	}
	if unit.Entrypoint != "" {
		_, _ = fmt.Fprintf(&sb, "Entrypoint=%s\n", sanitizeINIValue(unit.Entrypoint))
	}
	if unit.Exec != "" {
		_, _ = fmt.Fprintf(&sb, "Exec=%s\n", sanitizeINIValue(unit.Exec))
	}
	if unit.User != "" {
		_, _ = fmt.Fprintf(&sb, "User=%s\n", sanitizeINIValue(unit.User))
	}
	if unit.Group != "" {
		_, _ = fmt.Fprintf(&sb, "Group=%s\n", sanitizeINIValue(unit.Group))
	}
	if unit.ReadOnly {
		sb.WriteString("ReadOnly=true\n")
	}
	if unit.ReadOnlyTmpfs != nil {
		_, _ = fmt.Fprintf(&sb, "ReadOnlyTmpfs=%t\n", *unit.ReadOnlyTmpfs)
	}
	for _, capability := range unit.DropCapability {
		_, _ = fmt.Fprintf(&sb, "DropCapability=%s\n", sanitizeINIValue(capability))
	}
	for _, capability := range unit.AddCapability {
		_, _ = fmt.Fprintf(&sb, "AddCapability=%s\n", sanitizeINIValue(capability))
	}
	if unit.NoNewPrivileges {
		sb.WriteString("NoNewPrivileges=true\n")
	}
	for _, mount := range unit.Tmpfs {
		_, _ = fmt.Fprintf(&sb, "Tmpfs=%s\n", sanitizeINIValue(mount))
	}
	if unit.Memory != "" {
		_, _ = fmt.Fprintf(&sb, "Memory=%s\n", sanitizeINIValue(unit.Memory))
	}
	if unit.PidsLimit > 0 {
		_, _ = fmt.Fprintf(&sb, "PidsLimit=%d\n", unit.PidsLimit)
	}
	if unit.ShmSize != "" {
		_, _ = fmt.Fprintf(&sb, "ShmSize=%s\n", sanitizeINIValue(unit.ShmSize))
	}
	for _, limit := range unit.Ulimit {
		_, _ = fmt.Fprintf(&sb, "Ulimit=%s\n", sanitizeINIValue(limit))
	}
	for _, arg := range unit.PodmanArgs {
		_, _ = fmt.Fprintf(&sb, "PodmanArgs=%s\n", sanitizeINIValue(arg))
	}
	sb.WriteString("\n")

	// [Service] section
	sb.WriteString("[Service]\n")
	if unit.Restart != "" {
		_, _ = fmt.Fprintf(&sb, "Restart=%s\n", unit.Restart)
	} else {
		sb.WriteString("Restart=always\n")
	}
	if unit.TimeoutStopSec > 0 {
		_, _ = fmt.Fprintf(&sb, "TimeoutStopSec=%d\n", unit.TimeoutStopSec)
	}
	sb.WriteString("\n")

	// [Install] section
	sb.WriteString("[Install]\n")
	if unit.WantedBy != "" {
		_, _ = fmt.Fprintf(&sb, "WantedBy=%s\n", unit.WantedBy)
	} else {
		sb.WriteString("WantedBy=default.target\n")
	}

	return sb.String()
}

// GenerateNetworkFile generates a .network quadlet INI file
func GenerateNetworkFile(name string, dnsEnabled bool) string {
	var sb strings.Builder

	sb.WriteString("[Network]\n")
	if name != "" {
		_, _ = fmt.Fprintf(&sb, "NetworkName=%s\n", name)
	}
	if dnsEnabled {
		sb.WriteString("DNS=true\n")
	}

	return sb.String()
}

// GenerateVolumeFile generates a .volume quadlet INI file
func GenerateVolumeFile(name string) string {
	var sb strings.Builder

	sb.WriteString("[Volume]\n")
	if name != "" {
		_, _ = fmt.Fprintf(&sb, "VolumeName=%s\n", name)
	}

	return sb.String()
}
