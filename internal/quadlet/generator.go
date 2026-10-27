package quadlet

import (
	"fmt"
	"regexp"
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
	Exec            string
	PodmanArgs      []string
	Restart         string // systemd restart policy: always, on-failure
	TimeoutStopSec  int
	WantedBy        string
}

func quoteSystemdWord(s string, escapePercent bool) string {
	// A quoted word ends in '"', so a trailing backslash is escaped data, not a
	// line continuation, and is kept.
	s = stripINIControls(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	if escapePercent {
		s = strings.ReplaceAll(s, "%", "%%")
	}
	return `"` + s + `"`
}

// sanitizeINIValue makes s safe as an unquoted INI value. It removes newlines
// and control characters to prevent injection of additional systemd
// directives, and trailing backslashes because systemd and Quadlet treat a
// line that ends in '\' as continuing onto the next line, which would swallow
// the following directive.
func sanitizeINIValue(s string) string {
	return strings.TrimRight(stripINIControls(s), " \t\\")
}

// stripINIControls removes newlines, C0 and C1 control characters, DEL, and
// Unicode line/paragraph separators. Tabs are kept.
func stripINIControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') || (r >= 0x7F && r <= 0x9F) || r == '\u2028' || r == '\u2029' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var (
	validRestartPolicies = map[string]bool{
		"no":          true,
		"on-success":  true,
		"on-failure":  true,
		"on-abnormal": true,
		"on-watchdog": true,
		"on-abort":    true,
		"always":      true,
	}
	systemdUnitNameRegex  = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]{1,250}\.(target|service)$`)
	podmanObjectNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// Validate reports Restart and WantedBy values that systemd would reject or
// misinterpret. Empty values are valid; the generator applies defaults.
func (u *ContainerUnit) Validate() error {
	if u.Restart != "" && !validRestartPolicies[u.Restart] {
		return fmt.Errorf("invalid restart policy %q: must be one of always, no, on-abnormal, on-abort, on-failure, on-success, on-watchdog", u.Restart)
	}
	if u.WantedBy != "" {
		for _, unit := range strings.Split(u.WantedBy, " ") {
			if !systemdUnitNameRegex.MatchString(unit) {
				return fmt.Errorf("invalid WantedBy unit %q: must be space-separated .target or .service unit names", u.WantedBy)
			}
		}
	}
	return nil
}

// ValidateNetworkName reports whether name is a valid Podman network name.
func ValidateNetworkName(name string) error {
	if !podmanObjectNameRegex.MatchString(name) {
		return fmt.Errorf("invalid network name %q", name)
	}
	return nil
}

// ValidateVolumeName reports whether name is a valid Podman volume name.
func ValidateVolumeName(name string) error {
	if !podmanObjectNameRegex.MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
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
	if unit.Exec != "" {
		_, _ = fmt.Fprintf(&sb, "Exec=%s\n", sanitizeINIValue(unit.Exec))
	}
	for _, arg := range unit.PodmanArgs {
		_, _ = fmt.Fprintf(&sb, "PodmanArgs=%s\n", sanitizeINIValue(arg))
	}
	sb.WriteString("\n")

	// [Service] section
	sb.WriteString("[Service]\n")
	if unit.Restart != "" {
		_, _ = fmt.Fprintf(&sb, "Restart=%s\n", sanitizeINIValue(unit.Restart))
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
		_, _ = fmt.Fprintf(&sb, "WantedBy=%s\n", sanitizeINIValue(unit.WantedBy))
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
		_, _ = fmt.Fprintf(&sb, "NetworkName=%s\n", sanitizeINIValue(name))
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
		_, _ = fmt.Fprintf(&sb, "VolumeName=%s\n", sanitizeINIValue(name))
	}

	return sb.String()
}
