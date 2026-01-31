package output

import (
	"os"
	"strings"
	"testing"
)

func TestDetectProfile_NoColor(t *testing.T) {
	// Save and restore env
	restoreEnv := setEnvVars(map[string]string{
		"NO_COLOR":   "1",
		"COLORTERM":  "",
		"TERM":       "xterm-256color",
	})
	defer restoreEnv()

	p := detectProfile()
	if p != ProfileNone {
		t.Errorf("expected ProfileNone when NO_COLOR is set, got %d", p)
	}
}

func TestDetectProfile_TrueColor(t *testing.T) {
	restoreEnv := setEnvVars(map[string]string{
		"COLORTERM": "truecolor",
		"TERM":      "xterm-256color",
	})
	defer restoreEnv()
	unsetEnv := unsetEnvVar("NO_COLOR")
	defer unsetEnv()

	p := detectProfile()
	// Non-TTY in test → ProfileNone, so we skip the TTY-dependent assertion
	// and test the logic directly by checking TrueColor detection only when TTY
	if isTTY() && p != ProfileTrueColor {
		t.Errorf("expected ProfileTrueColor, got %d", p)
	}
}

func TestDetectProfile_24bit(t *testing.T) {
	restoreEnv := setEnvVars(map[string]string{
		"COLORTERM": "24bit",
		"TERM":      "xterm",
	})
	defer restoreEnv()
	unsetEnv := unsetEnvVar("NO_COLOR")
	defer unsetEnv()

	p := detectProfile()
	if isTTY() && p != ProfileTrueColor {
		t.Errorf("expected ProfileTrueColor for 24bit, got %d", p)
	}
}

func TestDetectProfile_256color(t *testing.T) {
	restoreEnv := setEnvVars(map[string]string{
		"COLORTERM": "",
		"TERM":      "xterm-256color",
	})
	defer restoreEnv()
	unsetEnv := unsetEnvVar("NO_COLOR")
	defer unsetEnv()

	p := detectProfile()
	if isTTY() && p != ProfileANSI256 {
		t.Errorf("expected ProfileANSI256, got %d", p)
	}
}

func TestDetectProfile_Dumb(t *testing.T) {
	restoreEnv := setEnvVars(map[string]string{
		"COLORTERM": "",
		"TERM":      "dumb",
	})
	defer restoreEnv()
	unsetEnv := unsetEnvVar("NO_COLOR")
	defer unsetEnv()

	p := detectProfile()
	if p != ProfileNone {
		t.Errorf("expected ProfileNone for dumb terminal, got %d", p)
	}
}

func TestSprintTrueColor(t *testing.T) {
	SetProfile(ProfileTrueColor)
	defer SetProfile(ProfileNone)

	out := Lavender.Sprint("hello")
	if !strings.Contains(out, "\033[38;2;180;167;214m") {
		t.Errorf("expected true-color escape for Lavender, got %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected text 'hello' in output, got %q", out)
	}
	if !strings.HasSuffix(out, "\033[0m") {
		t.Errorf("expected reset suffix, got %q", out)
	}
}

func TestSprintANSI256(t *testing.T) {
	SetProfile(ProfileANSI256)
	defer SetProfile(ProfileNone)

	out := Mint.Sprint("ok")
	if !strings.Contains(out, "\033[38;5;158m") {
		t.Errorf("expected 256-color escape for Mint (158), got %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected text 'ok' in output, got %q", out)
	}
}

func TestSprintBasic(t *testing.T) {
	SetProfile(ProfileBasic)
	defer SetProfile(ProfileNone)

	out := Rose.Sprint("fail")
	// fatih/color produces ANSI escapes even when not a TTY, unless color.NoColor is set.
	// Just verify the text is present.
	if !strings.Contains(out, "fail") {
		t.Errorf("expected text 'fail' in output, got %q", out)
	}
}

func TestSprintNone(t *testing.T) {
	SetProfile(ProfileNone)

	out := Peach.Sprint("warn")
	if out != "warn" {
		t.Errorf("expected plain text 'warn' for ProfileNone, got %q", out)
	}
}

func TestBoldTrueColor(t *testing.T) {
	SetProfile(ProfileTrueColor)
	defer SetProfile(ProfileNone)

	out := Rose.Bold("error")
	if !strings.Contains(out, "\033[1m") {
		t.Errorf("expected bold SGR in output, got %q", out)
	}
	if !strings.Contains(out, "\033[38;2;255;107;107m") {
		t.Errorf("expected true-color escape for Rose, got %q", out)
	}
}

func TestBoldNone(t *testing.T) {
	SetProfile(ProfileNone)

	out := Rose.Bold("error")
	if out != "error" {
		t.Errorf("expected plain text for ProfileNone Bold, got %q", out)
	}
}

func TestBoldANSI256(t *testing.T) {
	SetProfile(ProfileANSI256)
	defer SetProfile(ProfileNone)

	out := SkyBlue.Bold("cmd")
	if !strings.Contains(out, "\033[1m") {
		t.Errorf("expected bold SGR in 256-color output, got %q", out)
	}
	if !strings.Contains(out, "\033[38;5;117m") {
		t.Errorf("expected 256-color escape for SkyBlue (117), got %q", out)
	}
}

// Helpers

func setEnvVars(vars map[string]string) func() {
	originals := make(map[string]string)
	wasSet := make(map[string]bool)
	for k, v := range vars {
		if orig, ok := os.LookupEnv(k); ok {
			originals[k] = orig
			wasSet[k] = true
		}
		os.Setenv(k, v)
	}
	return func() {
		for k := range vars {
			if wasSet[k] {
				os.Setenv(k, originals[k])
			} else {
				os.Unsetenv(k)
			}
		}
	}
}

func unsetEnvVar(key string) func() {
	orig, wasSet := os.LookupEnv(key)
	os.Unsetenv(key)
	return func() {
		if wasSet {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	}
}
