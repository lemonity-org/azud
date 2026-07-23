package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestDetectProfileForCapabilities(t *testing.T) {
	tests := []struct {
		name string
		tty  bool
		env  map[string]string
		want ColorProfile
	}{
		{name: "NO_COLOR set empty", tty: true, env: map[string]string{"NO_COLOR": ""}, want: ProfileNone},
		{name: "NO_COLOR set value", tty: true, env: map[string]string{"NO_COLOR": "1"}, want: ProfileNone},
		{name: "CLICOLOR disabled", tty: true, env: map[string]string{"CLICOLOR": "0"}, want: ProfileNone},
		{name: "non TTY", tty: false, env: map[string]string{"COLORTERM": "truecolor"}, want: ProfileNone},
		{name: "dumb terminal", tty: true, env: map[string]string{"TERM": "dumb"}, want: ProfileNone},
		{name: "true color", tty: true, env: map[string]string{"COLORTERM": "truecolor"}, want: ProfileTrueColor},
		{name: "24 bit", tty: true, env: map[string]string{"COLORTERM": "24bit"}, want: ProfileTrueColor},
		{name: "256 color", tty: true, env: map[string]string{"TERM": "xterm-256color"}, want: ProfileANSI256},
		{name: "basic", tty: true, env: map[string]string{"TERM": "xterm"}, want: ProfileBasic},
		{name: "unset terminal", tty: true, env: map[string]string{}, want: ProfileBasic},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				value, ok := test.env[key]
				return value, ok
			}
			if got := detectProfileForCapabilities(test.tty, lookup); got != test.want {
				t.Fatalf("detectProfileForCapabilities() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestColorRenderingProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile ColorProfile
		bold    bool
		want    string
	}{
		{name: "plain", profile: ProfileNone, want: "state"},
		{name: "basic", profile: ProfileBasic, want: "\x1b[34mstate\x1b[0m"},
		{name: "basic bold", profile: ProfileBasic, bold: true, want: "\x1b[1;34mstate\x1b[0m"},
		{name: "256", profile: ProfileANSI256, want: "\x1b[38;5;20mstate\x1b[0m"},
		{name: "256 bold", profile: ProfileANSI256, bold: true, want: "\x1b[1;38;5;20mstate\x1b[0m"},
		{name: "true color", profile: ProfileTrueColor, want: "\x1b[38;2;0;45;206mstate\x1b[0m"},
		{name: "true color bold", profile: ProfileTrueColor, bold: true, want: "\x1b[1;38;2;0;45;206mstate\x1b[0m"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got string
			if test.bold {
				got = Blue.render("state", true, test.profile)
			} else {
				got = Blue.render("state", false, test.profile)
			}
			if got != test.want {
				t.Fatalf("render() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBufferWriterDefaultsToPlain(t *testing.T) {
	ResetProfile()
	t.Cleanup(ResetProfile)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")

	var buffer bytes.Buffer
	if got := detectProfileForWriter(&buffer); got != ProfileNone {
		t.Fatalf("buffer profile = %d, want ProfileNone", got)
	}
}

func TestSetProfileOverridesWriterDetection(t *testing.T) {
	SetProfile(ProfileTrueColor)
	t.Cleanup(ResetProfile)

	var buffer bytes.Buffer
	got := styleForWriter(&buffer, Green, "OK", true)
	if !strings.Contains(got, "\x1b[1;32m") {
		t.Fatalf("override did not produce theme-mapped ANSI output: %q", got)
	}
	if supportsUnicode(&buffer) {
		t.Fatal("a color override must not make a non-TTY writer Unicode-capable")
	}
}

func TestSupportsUnicodeForCapabilities(t *testing.T) {
	tests := []struct {
		name        string
		tty         bool
		platform    string
		term        string
		locale      string
		windowsANSI bool
		want        bool
	}{
		{name: "UTF-8 Unix TTY", tty: true, platform: "linux", term: "xterm", locale: "en_US.UTF-8", want: true},
		{name: "plain C locale", tty: true, platform: "linux", term: "xterm", locale: "C", want: false},
		{name: "dumb terminal", tty: true, platform: "linux", term: "dumb", locale: "C.UTF-8", want: false},
		{name: "pipe", tty: false, platform: "linux", term: "xterm", locale: "C.UTF-8", want: false},
		{name: "Windows Terminal", tty: true, platform: "windows", windowsANSI: true, want: true},
		{name: "legacy Windows console", tty: true, platform: "windows", windowsANSI: false, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := supportsUnicodeForCapabilities(test.tty, test.platform, test.term, test.locale, test.windowsANSI)
			if got != test.want {
				t.Fatalf("supportsUnicodeForCapabilities() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLegacyPaletteAliasesUseSemanticColors(t *testing.T) {
	if Lavender != Blue || Mint != Green || Peach != Yellow || Rose != Red || Pink != Red {
		t.Fatal("legacy aliases must map to the semantic palette")
	}
}
