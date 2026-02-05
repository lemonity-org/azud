package quadlet

import (
	"fmt"
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
	Exec            string
	PodmanArgs      []string
	Restart         string // systemd restart policy: always, on-failure
	TimeoutStopSec  int
	WantedBy        string
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
		sb.WriteString(fmt.Sprintf("Description=%s\n", sanitizeINIValue(unit.Description)))
	}
	for _, after := range unit.After {
		sb.WriteString(fmt.Sprintf("After=%s\n", sanitizeINIValue(after)))
	}
	for _, req := range unit.Requires {
		sb.WriteString(fmt.Sprintf("Requires=%s\n", sanitizeINIValue(req)))
	}
	sb.WriteString("\n")

	// [Container] section
	sb.WriteString("[Container]\n")
	if unit.Image != "" {
		sb.WriteString(fmt.Sprintf("Image=%s\n", sanitizeINIValue(unit.Image)))
	}
	if unit.ContainerName != "" {
		sb.WriteString(fmt.Sprintf("ContainerName=%s\n", sanitizeINIValue(unit.ContainerName)))
	}
	for key, value := range unit.Environment {
		sb.WriteString(fmt.Sprintf("Environment=%s=%s\n", sanitizeINIValue(key), sanitizeINIValue(value)))
	}
	for _, file := range unit.EnvironmentFile {
		sb.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", sanitizeINIValue(file)))
	}
	for _, port := range unit.PublishPort {
		sb.WriteString(fmt.Sprintf("PublishPort=%s\n", sanitizeINIValue(port)))
	}
	for _, vol := range unit.Volume {
		sb.WriteString(fmt.Sprintf("Volume=%s\n", sanitizeINIValue(vol)))
	}
	for _, net := range unit.Network {
		sb.WriteString(fmt.Sprintf("Network=%s\n", sanitizeINIValue(net)))
	}
	for key, value := range unit.Label {
		sb.WriteString(fmt.Sprintf("Label=%s=%s\n", sanitizeINIValue(key), sanitizeINIValue(value)))
	}
	if unit.HealthCmd != "" {
		sb.WriteString(fmt.Sprintf("HealthCmd=%s\n", sanitizeINIValue(unit.HealthCmd)))
	}
	if unit.HealthInterval != "" {
		sb.WriteString(fmt.Sprintf("HealthInterval=%s\n", sanitizeINIValue(unit.HealthInterval)))
	}
	if unit.Exec != "" {
		sb.WriteString(fmt.Sprintf("Exec=%s\n", sanitizeINIValue(unit.Exec)))
	}
	for _, arg := range unit.PodmanArgs {
		sb.WriteString(fmt.Sprintf("PodmanArgs=%s\n", sanitizeINIValue(arg)))
	}
	sb.WriteString("\n")

	// [Service] section
	sb.WriteString("[Service]\n")
	if unit.Restart != "" {
		sb.WriteString(fmt.Sprintf("Restart=%s\n", unit.Restart))
	} else {
		sb.WriteString("Restart=always\n")
	}
	if unit.TimeoutStopSec > 0 {
		sb.WriteString(fmt.Sprintf("TimeoutStopSec=%d\n", unit.TimeoutStopSec))
	}
	sb.WriteString("\n")

	// [Install] section
	sb.WriteString("[Install]\n")
	if unit.WantedBy != "" {
		sb.WriteString(fmt.Sprintf("WantedBy=%s\n", unit.WantedBy))
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
		sb.WriteString(fmt.Sprintf("NetworkName=%s\n", name))
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
		sb.WriteString(fmt.Sprintf("VolumeName=%s\n", name))
	}

	return sb.String()
}
