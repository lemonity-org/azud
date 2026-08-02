package proxy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/lemonity-org/azud/internal/podman"
)

const (
	// CaddyAdminListen is deliberately loopback-only inside azud-proxy. Azud
	// reaches it with podman exec; the admin port is never published.
	CaddyAdminListen = "127.0.0.1:2019"

	CaddyRuntimeUser          = "1000"
	CaddyRuntimeGroup         = "1000"
	CaddyMemoryLimit          = "512m"
	CaddyMemorySwapLimit      = "512m"
	CaddyCPULimit             = "4"
	CaddyPidsLimit            = 256
	CaddyShmSize              = "16m"
	CaddyNofileLimit          = int64(65536)
	CaddyStopTimeout          = 30
	CaddyRuntimeRevision      = "2"
	CaddyRecoveryConfigFile   = "/config/caddy/azud.json"
	CaddyRuntimeEntrypoint    = "/bin/sh"
	caddyMemoryLimitBytes     = int64(512 * 1024 * 1024)
	caddyCPULimitNanoCPUs     = int64(4 * 1_000_000_000)
	caddyShmSizeBytes         = int64(16 * 1024 * 1024)
	caddyRuntimeRevisionLabel = "azud.proxy.runtime"
)

var caddyTmpfs = []string{
	"/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777",
	"/run:rw,noexec,nosuid,nodev,size=8m,mode=0755",
}

var caddyCommand = []string{
	"-eu", "-c",
	"if [ -s " + CaddyRecoveryConfigFile + " ]; then exec caddy run --config " + CaddyRecoveryConfigFile + "; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile; fi",
}

const caddyRecoveryWriteScript = "umask 077; mkdir -p /config/caddy; tmp=\"$(mktemp /config/caddy/.azud.json.XXXXXX)\"; trap 'rm -f \"$tmp\"' EXIT HUP INT TERM; cat > \"$tmp\"; test -s \"$tmp\"; chmod 600 \"$tmp\"; mv -f \"$tmp\" " + CaddyRecoveryConfigFile + "; sync; trap - EXIT HUP INT TERM"

// NewCaddyContainerConfig returns the single authoritative runtime shape used
// by both imperative Podman and generated Quadlet deployments.
func NewCaddyContainerConfig(httpPort, httpsPort int, hostNetwork bool) *podman.ContainerConfig {
	config := &podman.ContainerConfig{
		Name:    CaddyContainerName,
		Image:   CaddyImage,
		Detach:  true,
		Restart: "unless-stopped",
		Volumes: []string{
			"caddy_data:/data:U",
			"caddy_config:/config:U",
		},
		Labels: map[string]string{
			"azud.managed":            "true",
			"azud.type":               "proxy",
			caddyRuntimeRevisionLabel: CaddyRuntimeRevision,
		},
		Command: append([]string(nil), caddyCommand...),
		// Pin the process entrypoint explicitly so the recovery selector does not
		// depend on mutable image command metadata.
		Entrypoint: CaddyRuntimeEntrypoint,
		Env: map[string]string{
			"CADDY_ADMIN": CaddyAdminListen,
		},
		User:                 CaddyRuntimeUser + ":" + CaddyRuntimeGroup,
		ReadOnly:             true,
		DisableReadOnlyTmpfs: true,
		CapDrop:              []string{"ALL"},
		CapAdd:               []string{"NET_BIND_SERVICE"},
		NoNewPrivileges:      true,
		Tmpfs:                append([]string(nil), caddyTmpfs...),
		PidsLimit:            CaddyPidsLimit,
		ShmSize:              CaddyShmSize,
		Memory:               CaddyMemoryLimit,
		MemorySwap:           CaddyMemorySwapLimit,
		CPUs:                 CaddyCPULimit,
		Ulimits:              []string{fmt.Sprintf("nofile=%d:%d", CaddyNofileLimit, CaddyNofileLimit)},
		StopTimeout:          CaddyStopTimeout,
	}
	if hostNetwork {
		config.Network = "host"
	} else {
		config.Network = "azud"
		config.Ports = []string{
			fmt.Sprintf("%d:%d", httpPort, CaddyHTTPPort),
			fmt.Sprintf("%d:%d", httpsPort, CaddyHTTPSPort),
		}
	}
	return config
}

// newCaddyRecoveryWriterConfig returns the minimal one-shot runtime used to
// stream an accepted host snapshot into the protected caddy_config volume.
// It has no network, ports, ambient capabilities, or writable root filesystem.
func newCaddyRecoveryWriterConfig() *podman.ContainerConfig {
	return &podman.ContainerConfig{
		Image:                CaddyImage,
		Command:              []string{"-eu", "-c", caddyRecoveryWriteScript},
		Entrypoint:           CaddyRuntimeEntrypoint,
		Volumes:              []string{"caddy_config:/config:U"},
		Network:              "none",
		User:                 CaddyRuntimeUser + ":" + CaddyRuntimeGroup,
		ReadOnly:             true,
		DisableReadOnlyTmpfs: true,
		CapDrop:              []string{"ALL"},
		NoNewPrivileges:      true,
		PidsLimit:            32,
		ShmSize:              "4m",
		Memory:               "64m",
		MemorySwap:           "64m",
		CPUs:                 "1",
		Ulimits:              []string{"nofile=1024:1024"},
		NoHealthcheck:        true,
		Remove:               true,
	}
}

type caddyInspect struct {
	ImageName     string       `json:"ImageName"`
	EffectiveCaps []string     `json:"EffectiveCaps"`
	BoundingCaps  []string     `json:"BoundingCaps"`
	Mounts        []caddyMount `json:"Mounts"`
	Config        struct {
		Image       string            `json:"Image"`
		User        string            `json:"User"`
		Env         []string          `json:"Env"`
		Labels      map[string]string `json:"Labels"`
		Cmd         []string          `json:"Cmd"`
		Entrypoint  inspectArgv       `json:"Entrypoint"`
		StopTimeout int               `json:"StopTimeout"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode    string                        `json:"NetworkMode"`
		ReadonlyRootfs bool                          `json:"ReadonlyRootfs"`
		CapAdd         []string                      `json:"CapAdd"`
		CapDrop        []string                      `json:"CapDrop"`
		SecurityOpt    []string                      `json:"SecurityOpt"`
		PidsLimit      int64                         `json:"PidsLimit"`
		Memory         int64                         `json:"Memory"`
		MemorySwap     int64                         `json:"MemorySwap"`
		NanoCPUs       int64                         `json:"NanoCpus"`
		ShmSize        int64                         `json:"ShmSize"`
		PortBindings   map[string][]caddyPortBinding `json:"PortBindings"`
		Tmpfs          map[string]string             `json:"Tmpfs"`
		ReadonlyTmpfs  bool                          `json:"ReadonlyTmpfs"`
		ReadOnlyTmpfs  bool                          `json:"ReadOnlyTmpfs"`
		Ulimits        []caddyUlimit                 `json:"Ulimits"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports    map[string][]caddyPortBinding `json:"Ports"`
		Networks map[string]json.RawMessage    `json:"Networks"`
	} `json:"NetworkSettings"`
}

type caddyMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type caddyUlimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type caddyPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// Podman has emitted Config.Entrypoint as both a string and a JSON argv array
// across inspect schema versions. Normalize both without weakening the exact
// runtime comparison.
type inspectArgv []string

func (a *inspectArgv) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*a = values
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == "" {
		*a = nil
	} else {
		*a = []string{value}
	}
	return nil
}

func inspectCaddyRuntime(raw string, hostNetwork bool, httpPort, httpsPort int) []string {
	var payload []caddyInspect
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []string{"invalid Podman inspect JSON: " + err.Error()}
	}
	if len(payload) != 1 {
		return []string{fmt.Sprintf("expected one Podman inspect object, got %d", len(payload))}
	}
	got := payload[0]
	var drift []string
	add := func(reason string) { drift = append(drift, reason) }

	if got.Config.Labels[caddyRuntimeRevisionLabel] != CaddyRuntimeRevision {
		add("runtime revision label is missing or stale")
	}
	if got.Config.User != CaddyRuntimeUser+":"+CaddyRuntimeGroup {
		add("container user is not the dedicated non-root UID/GID")
	}
	if !got.HostConfig.ReadonlyRootfs {
		add("root filesystem is writable")
	}
	if !caddyCapabilitiesAreConfined(got.HostConfig.CapDrop, got.HostConfig.CapAdd, got.EffectiveCaps, got.BoundingCaps) {
		add("capability set is not confined to NET_BIND_SERVICE")
	}
	if !containsNormalized(got.HostConfig.SecurityOpt, "no-new-privileges") {
		add("no-new-privileges is disabled")
	}
	if got.HostConfig.PidsLimit != CaddyPidsLimit {
		add(fmt.Sprintf("pids limit is %d, expected %d", got.HostConfig.PidsLimit, CaddyPidsLimit))
	}
	if got.HostConfig.Memory != caddyMemoryLimitBytes {
		add("memory limit is missing or incorrect")
	}
	if got.HostConfig.MemorySwap != caddyMemoryLimitBytes {
		add("memory plus swap limit is missing or incorrect")
	}
	if got.HostConfig.NanoCPUs != caddyCPULimitNanoCPUs {
		add("CPU limit is missing or incorrect")
	}
	if got.HostConfig.ShmSize != caddyShmSizeBytes {
		add("shared-memory limit is missing or incorrect")
	}
	if got.Config.StopTimeout != CaddyStopTimeout {
		add(fmt.Sprintf("stop timeout is %d, expected %d", got.Config.StopTimeout, CaddyStopTimeout))
	}
	if !caddyUlimitsAreConfined(got.HostConfig.Ulimits) {
		add("ulimit set does not contain exact nofile=65536:65536 with only an optional safe NPROC limit")
	}
	if got.HostConfig.ReadonlyTmpfs || got.HostConfig.ReadOnlyTmpfs {
		add("Podman automatic read-only tmpfs mounts are enabled")
	}
	for _, spec := range caddyTmpfs {
		path, options, _ := strings.Cut(spec, ":")
		actual, ok := got.HostConfig.Tmpfs[path]
		if !ok || !tmpfsContainsOptions(actual, options) {
			add("bounded tmpfs is missing or incorrect for " + path)
		}
	}
	if !hasExactEnv(got.Config.Env, "CADDY_ADMIN", CaddyAdminListen) {
		add("CADDY_ADMIN is not container-loopback-only")
	}
	if !caddyMountsAreExact(got.Mounts) {
		add("mount set is not exactly the writable caddy_data and caddy_config named volumes")
	}
	expectedPorts := map[string]int{
		fmt.Sprintf("%d/tcp", CaddyHTTPPort):  httpPort,
		fmt.Sprintf("%d/tcp", CaddyHTTPSPort): httpsPort,
	}
	if hostNetwork {
		expectedPorts = map[string]int{}
	}
	if !activePortBindingsEqual(got.HostConfig.PortBindings, expectedPorts) {
		add(fmt.Sprintf("published port bindings are %v, expected %v", activePortDescriptions(got.HostConfig.PortBindings), expectedPortDescriptions(expectedPorts)))
	}
	if unexpected := unexpectedActivePortKeys(got.NetworkSettings.Ports, expectedPorts); len(unexpected) > 0 {
		add(fmt.Sprintf("live network has unexpected published ports: %v", unexpected))
	}
	expectedNetwork := "azud"
	if hostNetwork {
		expectedNetwork = "host"
	}
	if !caddyNetworkIsExact(got.HostConfig.NetworkMode, got.NetworkSettings.Networks, expectedNetwork, hostNetwork) {
		add(fmt.Sprintf("network mode/membership is %q/%v, expected only %q", got.HostConfig.NetworkMode, networkNames(got.NetworkSettings.Networks), expectedNetwork))
	}
	if !reflect.DeepEqual(got.Config.Cmd, caddyCommand) {
		add("startup command does not use the protected Azud recovery config")
	}
	if !reflect.DeepEqual([]string(got.Config.Entrypoint), []string{CaddyRuntimeEntrypoint}) {
		add("container entrypoint does not execute the protected recovery selector")
	}
	if !caddyImageReferencesArePinned(got.ImageName, got.Config.Image) {
		add(fmt.Sprintf("Caddy image references differ from the pinned digest: ImageName=%q Config.Image=%q", got.ImageName, got.Config.Image))
	}

	sort.Strings(drift)
	return drift
}

func caddyMountsAreExact(mounts []caddyMount) bool {
	wantVolumes := map[string]string{
		"/data":   "caddy_data",
		"/config": "caddy_config",
	}
	seenVolumes := make(map[string]struct{}, len(wantVolumes))
	seenTmpfs := make(map[string]struct{}, len(caddyTmpfs))
	for _, mount := range mounts {
		switch strings.ToLower(mount.Type) {
		case "volume":
			wantName, ok := wantVolumes[mount.Destination]
			if !ok || mount.Name != wantName || !mount.RW {
				return false
			}
			if _, duplicate := seenVolumes[mount.Destination]; duplicate {
				return false
			}
			seenVolumes[mount.Destination] = struct{}{}
		case "tmpfs":
			if mount.Destination != "/tmp" && mount.Destination != "/run" {
				return false
			}
			if _, duplicate := seenTmpfs[mount.Destination]; duplicate {
				return false
			}
			seenTmpfs[mount.Destination] = struct{}{}
		default:
			return false
		}
	}
	// Podman versions differ on whether explicit tmpfs entries also appear in
	// top-level Mounts; HostConfig.Tmpfs is validated separately above. Accept
	// either representation, but never a partial or additional top-level set.
	return len(seenVolumes) == len(wantVolumes) && (len(seenTmpfs) == 0 || len(seenTmpfs) == len(caddyTmpfs))
}

func caddyUlimitsAreConfined(limits []caddyUlimit) bool {
	if len(limits) < 1 || len(limits) > 2 {
		return false
	}
	seenNofile := false
	seenNproc := false
	for _, limit := range limits {
		name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(limit.Name)), "RLIMIT_")
		switch name {
		case "NOFILE":
			if seenNofile || limit.Soft != CaddyNofileLimit || limit.Hard != CaddyNofileLimit {
				return false
			}
			seenNofile = true
		case "NPROC":
			// Podman 5.4 rootless adds the caller's process limit to inspect
			// even when only NOFILE was requested. It is an additional ceiling,
			// never a replacement for the exact requested limit. The exact cgroup
			// PidsLimit is independently verified above.
			if seenNproc || limit.Soft < 0 || limit.Hard < limit.Soft {
				return false
			}
			seenNproc = true
		default:
			return false
		}
	}
	return seenNofile
}

func caddyCapabilitiesAreConfined(capDrop, capAdd, effective, bounding []string) bool {
	wanted := []string{"NET_BIND_SERVICE"}
	intentIsExact := containsCapability(capDrop, "ALL") && capabilitySetEquals(capAdd, wanted)
	runtimeProofIsExact := effective != nil && bounding != nil &&
		capabilitySetEquals(effective, wanted) && capabilitySetEquals(bounding, wanted)

	// When Podman exposes live capability fields, they are stronger evidence
	// than its Docker-compatible HostConfig projection and must not show drift.
	if effective != nil && !capabilitySetEquals(effective, wanted) {
		return false
	}
	if bounding != nil && !capabilitySetEquals(bounding, wanted) {
		return false
	}
	return intentIsExact || runtimeProofIsExact
}

func caddyNetworkIsExact(networkMode string, networks map[string]json.RawMessage, expected string, hostNetwork bool) bool {
	if hostNetwork {
		return networkMode == "host"
	}
	if networkMode != expected && networkMode != "bridge" {
		return false
	}
	if len(networks) != 1 {
		return false
	}
	membership, ok := networks[expected]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(membership))
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")
}

func networkNames(networks map[string]json.RawMessage) []string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func caddyImageReferencesArePinned(references ...string) bool {
	seen := false
	for _, reference := range references {
		if reference == "" {
			continue
		}
		seen = true
		if reference != CaddyImage && reference != caddyCanonicalDigestReference() {
			return false
		}
	}
	return seen
}

func caddyCanonicalDigestReference() string {
	name, digest, ok := strings.Cut(CaddyImage, "@")
	if !ok {
		return ""
	}
	if tag := strings.LastIndex(name, ":"); tag > strings.LastIndex(name, "/") {
		name = name[:tag]
	}
	return name + "@" + digest
}

func activePortBindingsEqual(bindings map[string][]caddyPortBinding, expected map[string]int) bool {
	if len(activePortKeys(bindings)) != len(expected) {
		return false
	}
	for containerPort, hostPort := range expected {
		values := bindings[containerPort]
		if len(values) != 1 {
			return false
		}
		parsed, err := strconv.Atoi(values[0].HostPort)
		if err != nil || parsed != hostPort {
			return false
		}
		switch values[0].HostIP {
		case "", "0.0.0.0", "::": // safe: accepted inspect encodings for the intentional public HTTP/HTTPS publish
		default:
			return false
		}
	}
	return true
}

func activePortKeys(bindings map[string][]caddyPortBinding) []string {
	keys := make([]string, 0, len(bindings))
	for port, values := range bindings {
		if len(values) > 0 {
			keys = append(keys, port)
		}
	}
	sort.Strings(keys)
	return keys
}

func activePortDescriptions(bindings map[string][]caddyPortBinding) []string {
	var descriptions []string
	for _, containerPort := range activePortKeys(bindings) {
		for _, binding := range bindings[containerPort] {
			descriptions = append(descriptions, fmt.Sprintf("%s->%s:%s", containerPort, binding.HostIP, binding.HostPort))
		}
	}
	return descriptions
}

func expectedPortDescriptions(expected map[string]int) []string {
	keys := make([]string, 0, len(expected))
	for containerPort := range expected {
		keys = append(keys, containerPort)
	}
	sort.Strings(keys)
	descriptions := make([]string, 0, len(keys))
	for _, containerPort := range keys {
		descriptions = append(descriptions, fmt.Sprintf("%s->%d", containerPort, expected[containerPort]))
	}
	return descriptions
}

func unexpectedActivePortKeys(bindings map[string][]caddyPortBinding, expected map[string]int) []string {
	var unexpected []string
	for _, port := range activePortKeys(bindings) {
		if _, ok := expected[port]; !ok {
			unexpected = append(unexpected, port)
		}
	}
	return unexpected
}

func containsCapability(values []string, wanted string) bool {
	wanted = normalizeCapability(wanted)
	for _, value := range values {
		if normalizeCapability(value) == wanted {
			return true
		}
	}
	return false
}

func capabilitySetEquals(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for _, wanted := range expected {
		if !containsCapability(actual, wanted) {
			return false
		}
	}
	return true
}

func normalizeCapability(value string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value)), "CAP_")
}

func containsNormalized(values []string, wanted string) bool {
	wanted = strings.ToLower(wanted)
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == wanted || normalized == wanted+"=true" {
			return true
		}
	}
	return false
}

func hasExactEnv(values []string, key, wanted string) bool {
	prefix := key + "="
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			count++
			if value != prefix+wanted {
				return false
			}
		}
	}
	return count == 1
}

func tmpfsContainsOptions(actual, wanted string) bool {
	actualSet := make(map[string]struct{})
	for _, option := range strings.Split(actual, ",") {
		actualSet[strings.TrimSpace(option)] = struct{}{}
	}
	for _, option := range strings.Split(wanted, ",") {
		option = strings.TrimSpace(option)
		if _, ok := actualSet[option]; ok {
			continue
		}
		if strings.HasPrefix(option, "mode=0") {
			if _, ok := actualSet["mode="+strings.TrimPrefix(option, "mode=0")]; ok {
				continue
			}
		}
		return false
	}
	return true
}
