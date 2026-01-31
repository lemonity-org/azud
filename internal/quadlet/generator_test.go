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
		"Environment=PORT=3000",
		"EnvironmentFile=/etc/secrets.env",
		"PublishPort=8080:3000",
		"Volume=/data:/app/data",
		"Network=azud",
		"Label=azud.managed=true",
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
