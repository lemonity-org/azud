package quadlet

import (
	"strings"
	"testing"
)

func TestGenerateContainerFile_Minimal(t *testing.T) {
	unit := &ContainerUnit{
		Image: "nginx:latest",
	}

	result := GenerateContainerFile(unit)

	if !strings.Contains(result, "[Unit]") {
		t.Error("expected [Unit] section")
	}
	if !strings.Contains(result, "[Container]") {
		t.Error("expected [Container] section")
	}
	if !strings.Contains(result, "Image=nginx:latest") {
		t.Error("expected Image=nginx:latest")
	}
	if !strings.Contains(result, "[Service]") {
		t.Error("expected [Service] section")
	}
	if !strings.Contains(result, "Restart=always") {
		t.Error("expected default Restart=always")
	}
	if !strings.Contains(result, "[Install]") {
		t.Error("expected [Install] section")
	}
	if !strings.Contains(result, "WantedBy=default.target") {
		t.Error("expected default WantedBy=default.target")
	}
}

func TestGenerateContainerFile_Full(t *testing.T) {
	unit := &ContainerUnit{
		Description:   "My Service",
		After:         []string{"network-online.target"},
		Requires:      []string{"network-online.target"},
		Image:         "myapp:latest",
		ContainerName: "myapp",
		Environment: map[string]string{
			"PORT": "3000",
		},
		EnvironmentFile: []string{"/etc/secrets.env"},
		PublishPort:     []string{"8080:3000"},
		Volume:          []string{"/data:/app/data"},
		Network:         []string{"azud"},
		Label:           map[string]string{"azud.managed": "true"},
		HealthCmd:       "echo ok",
		HealthInterval:  "10s",
		Exec:            "/app/start",
		PodmanArgs:      []string{"--memory=512m"},
		Restart:         "on-failure",
		TimeoutStopSec:  30,
		WantedBy:        "multi-user.target",
	}

	result := GenerateContainerFile(unit)

	checks := []string{
		"Description=My Service",
		"After=network-online.target",
		"Requires=network-online.target",
		"Image=myapp:latest",
		"ContainerName=myapp",
		`Environment="PORT=3000"`,
		`EnvironmentFile="/etc/secrets.env"`,
		"PublishPort=8080:3000",
		"Volume=/data:/app/data",
		"Network=azud",
		`Label="azud.managed=true"`,
		"HealthCmd=echo ok",
		"HealthInterval=10s",
		"Exec=/app/start",
		"PodmanArgs=--memory=512m",
		"Restart=on-failure",
		"TimeoutStopSec=30",
		"WantedBy=multi-user.target",
	}

	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("expected %q in output, got:\n%s", check, result)
		}
	}
}

func TestGenerateContainerFileQuotesAndOrdersEnvironment(t *testing.T) {
	unit := &ContainerUnit{Image: "app", Environment: map[string]string{
		"Z_LAST":  `spaces " quotes \\ and 100%`,
		"A_FIRST": "one",
	}}
	result := GenerateContainerFile(unit)
	first := `Environment="A_FIRST=one"`
	last := `Environment="Z_LAST=spaces \" quotes \\\\ and 100%%"`
	if !strings.Contains(result, first) || !strings.Contains(result, last) {
		t.Fatalf("environment was not safely encoded:\n%s", result)
	}
	if strings.Index(result, first) > strings.Index(result, last) {
		t.Fatalf("environment output is not deterministic:\n%s", result)
	}
}

func TestGenerateContainerFile_DefaultRestart(t *testing.T) {
	unit := &ContainerUnit{
		Image: "nginx:latest",
	}

	result := GenerateContainerFile(unit)

	if !strings.Contains(result, "Restart=always") {
		t.Error("expected default Restart=always when no restart policy specified")
	}
}

func TestGenerateContainerFile_CustomRestart(t *testing.T) {
	unit := &ContainerUnit{
		Image:   "nginx:latest",
		Restart: "on-failure",
	}

	result := GenerateContainerFile(unit)

	if !strings.Contains(result, "Restart=on-failure") {
		t.Error("expected Restart=on-failure")
	}
	if strings.Contains(result, "Restart=always") {
		t.Error("should not contain default Restart=always when custom is set")
	}
}

func TestGenerateContainerFile_DefaultWantedBy(t *testing.T) {
	unit := &ContainerUnit{
		Image: "nginx:latest",
	}

	result := GenerateContainerFile(unit)

	if !strings.Contains(result, "WantedBy=default.target") {
		t.Error("expected default WantedBy=default.target")
	}
}

func TestGenerateContainerFile_CustomWantedBy(t *testing.T) {
	unit := &ContainerUnit{
		Image:    "nginx:latest",
		WantedBy: "multi-user.target",
	}

	result := GenerateContainerFile(unit)

	if !strings.Contains(result, "WantedBy=multi-user.target") {
		t.Error("expected WantedBy=multi-user.target")
	}
}

func TestGenerateContainerFile_INIFormat(t *testing.T) {
	unit := &ContainerUnit{
		Description: "Test",
		Image:       "test:latest",
	}

	result := GenerateContainerFile(unit)

	// Verify section ordering
	unitIdx := strings.Index(result, "[Unit]")
	containerIdx := strings.Index(result, "[Container]")
	serviceIdx := strings.Index(result, "[Service]")
	installIdx := strings.Index(result, "[Install]")

	if unitIdx >= containerIdx {
		t.Error("[Unit] must come before [Container]")
	}
	if containerIdx >= serviceIdx {
		t.Error("[Container] must come before [Service]")
	}
	if serviceIdx >= installIdx {
		t.Error("[Service] must come before [Install]")
	}
}

func TestGenerateNetworkFile(t *testing.T) {
	result := GenerateNetworkFile("azud", true)

	if !strings.Contains(result, "[Network]") {
		t.Error("expected [Network] section")
	}
	if !strings.Contains(result, "NetworkName=azud") {
		t.Error("expected NetworkName=azud")
	}
	if !strings.Contains(result, "DNS=true") {
		t.Error("expected DNS=true")
	}
}

func TestGenerateNetworkFile_NoDNS(t *testing.T) {
	result := GenerateNetworkFile("mynet", false)

	if !strings.Contains(result, "NetworkName=mynet") {
		t.Error("expected NetworkName=mynet")
	}
	if strings.Contains(result, "DNS=true") {
		t.Error("should not contain DNS=true when disabled")
	}
}

func TestGenerateNetworkFile_EmptyName(t *testing.T) {
	result := GenerateNetworkFile("", false)

	if !strings.Contains(result, "[Network]") {
		t.Error("expected [Network] section")
	}
	if strings.Contains(result, "NetworkName=") {
		t.Error("should not contain NetworkName when empty")
	}
}

func TestGenerateVolumeFile(t *testing.T) {
	result := GenerateVolumeFile("mydata")

	if !strings.Contains(result, "[Volume]") {
		t.Error("expected [Volume] section")
	}
	if !strings.Contains(result, "VolumeName=mydata") {
		t.Error("expected VolumeName=mydata")
	}
}

func TestGenerateVolumeFile_EmptyName(t *testing.T) {
	result := GenerateVolumeFile("")

	if !strings.Contains(result, "[Volume]") {
		t.Error("expected [Volume] section")
	}
	if strings.Contains(result, "VolumeName=") {
		t.Error("should not contain VolumeName when empty")
	}
}

func TestGenerateContainerFile_SanitizesRestartAndWantedBy(t *testing.T) {
	injected := "always\n[Service]\nExecStartPre=/bin/sh -c id\r\u0085\u2028"
	unit := &ContainerUnit{
		Image:    "nginx:latest",
		Restart:  injected,
		WantedBy: injected,
	}

	result := GenerateContainerFile(unit)

	assertNoInjectedDirectives(t, result, []string{"[Unit]", "[Container]", "[Service]", "[Install]"})
	if !strings.Contains(result, "Restart=always[Service]ExecStartPre=/bin/sh -c id\n") {
		t.Errorf("expected Restart value on one line, got:\n%s", result)
	}
	if !strings.Contains(result, "WantedBy=always[Service]ExecStartPre=/bin/sh -c id\n") {
		t.Errorf("expected WantedBy value on one line, got:\n%s", result)
	}
}

func TestGenerateNetworkAndVolumeFile_SanitizeName(t *testing.T) {
	network := GenerateNetworkFile("azud\n[Network]\nDNS=false", true)
	assertNoInjectedDirectives(t, network, []string{"[Network]"})
	if want := "[Network]\nNetworkName=azud[Network]DNS=false\nDNS=true\n"; network != want {
		t.Errorf("network file = %q, want %q", network, want)
	}

	volume := GenerateVolumeFile("data\n[Volume]\nDriver=evil\\")
	assertNoInjectedDirectives(t, volume, []string{"[Volume]"})
	if want := "[Volume]\nVolumeName=data[Volume]Driver=evil\n"; volume != want {
		t.Errorf("volume file = %q, want %q", volume, want)
	}
}

func TestGenerateContainerFile_StripsTrailingBackslash(t *testing.T) {
	unit := &ContainerUnit{
		Image:       "nginx:latest",
		HealthCmd:   "check \\",
		Exec:        "sleep 1 \\ \t\\\\",
		PodmanArgs:  []string{"--read-only"},
		Environment: map[string]string{"WINDOWS_PATH": `C:\`},
	}

	result := GenerateContainerFile(unit)

	for _, line := range strings.Split(result, "\n") {
		if strings.HasSuffix(line, `\`) {
			t.Errorf("line ends in a continuation backslash: %q", line)
		}
	}
	for _, want := range []string{
		"HealthCmd=check\n",
		"Exec=sleep 1\nPodmanArgs=--read-only\n",
		// A quoted word keeps a trailing backslash as escaped data.
		`Environment="WINDOWS_PATH=C:\\"` + "\n",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in:\n%s", want, result)
		}
	}
}

func TestContainerUnitValidate(t *testing.T) {
	tests := []struct {
		name     string
		restart  string
		wantedBy string
		wantErr  bool
	}{
		{name: "defaults"},
		{name: "always", restart: "always", wantedBy: "default.target"},
		{name: "on-failure", restart: "on-failure", wantedBy: "multi-user.target"},
		{name: "no", restart: "no"},
		{name: "on-success", restart: "on-success"},
		{name: "on-abnormal", restart: "on-abnormal"},
		{name: "on-watchdog", restart: "on-watchdog"},
		{name: "on-abort", restart: "on-abort"},
		{name: "multiple wanted-by units", wantedBy: "default.target multi-user.target"},
		{name: "service wanted-by", wantedBy: "app@1.service"},
		{name: "unknown restart", restart: "unless-stopped", wantErr: true},
		{name: "quoted restart", restart: `"always"`, wantErr: true},
		{name: "injected restart", restart: "always\n[Service]", wantErr: true},
		{name: "wanted-by without unit type", wantedBy: "default", wantErr: true},
		{name: "wanted-by socket", wantedBy: "app.socket", wantErr: true},
		{name: "injected wanted-by", wantedBy: "default.target\nExecStartPre=id", wantErr: true},
		{name: "empty wanted-by element", wantedBy: "default.target  multi-user.target", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&ContainerUnit{Restart: tt.restart, WantedBy: tt.wantedBy}).Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidateNetworkAndVolumeNames(t *testing.T) {
	for _, name := range []string{"azud", "caddy_data", "app-1.data"} {
		if err := ValidateNetworkName(name); err != nil {
			t.Errorf("ValidateNetworkName(%q) = %v", name, err)
		}
		if err := ValidateVolumeName(name); err != nil {
			t.Errorf("ValidateVolumeName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "-azud", ".hidden", "azud\n[Network]", "data\n", "a b", "a/b", `a\`} {
		if err := ValidateNetworkName(name); err == nil {
			t.Errorf("ValidateNetworkName(%q) unexpectedly succeeded", name)
		}
		if err := ValidateVolumeName(name); err == nil {
			t.Errorf("ValidateVolumeName(%q) unexpectedly succeeded", name)
		}
	}
}

// assertNoInjectedDirectives fails when a section header does not appear
// exactly once, or when a line starts with a directive that only an injected
// value could produce. It checks line prefixes: a sanitized value can still
// contain the payload text on its own line.
func assertNoInjectedDirectives(t *testing.T, result string, sections []string) {
	t.Helper()
	lines := strings.Split(result, "\n")
	for _, section := range sections {
		count := 0
		for _, line := range lines {
			if strings.TrimSpace(line) == section {
				count++
			}
		}
		if count != 1 {
			t.Errorf("section %s appears %d times:\n%s", section, count, result)
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, injected := range []string{"ExecStartPre=", "DNS=false", "Driver="} {
			if strings.HasPrefix(trimmed, injected) {
				t.Errorf("injected directive line %q:\n%s", line, result)
			}
		}
	}
}
