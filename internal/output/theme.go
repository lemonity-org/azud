package output

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
)

// ColorProfile represents the level of color support available.
type ColorProfile int

const (
	ProfileNone     ColorProfile = iota // No color (NO_COLOR, non-TTY, dumb terminal)
	ProfileBasic                        // 16 basic ANSI colors
	ProfileANSI256                      // 256-color palette
	ProfileTrueColor                    // 24-bit RGB true color
)

// Symbols used throughout the CLI output.
const (
	SymInfo    = "●"
	SymSuccess = "✓"
	SymWarn    = "▲"
	SymError   = "✗"
	SymDebug   = "◦"
	SymCommand = "▸"
	SymHeader  = "◆"
	SymHost    = "◈"
	SymLine    = "───"
	SymFilled  = "█"
	SymPending = "○"
)

// PastelColor holds color values for every supported profile level.
type PastelColor struct {
	R, G, B   uint8
	ANSI256   uint8
	ANSIBasic color.Attribute
}

// Sprint returns text colored according to the active profile.
func (c PastelColor) Sprint(text string) string {
	return colorize(c, text, false)
}

// Bold returns bold-colored text according to the active profile.
func (c PastelColor) Bold(text string) string {
	return colorize(c, text, true)
}

// Palette — pastel "Bubblegum" colors.
var (
	Lavender = PastelColor{R: 0xB4, G: 0xA7, B: 0xD6, ANSI256: 146, ANSIBasic: color.FgCyan}
	Mint     = PastelColor{R: 0xA8, G: 0xE6, B: 0xCF, ANSI256: 158, ANSIBasic: color.FgGreen}
	Peach    = PastelColor{R: 0xFF, G: 0xDA, B: 0xB9, ANSI256: 223, ANSIBasic: color.FgYellow}
	Rose     = PastelColor{R: 0xFF, G: 0x6B, B: 0x6B, ANSI256: 210, ANSIBasic: color.FgRed}
	SkyBlue  = PastelColor{R: 0x89, G: 0xCF, B: 0xF0, ANSI256: 117, ANSIBasic: color.FgCyan}
	Mauve    = PastelColor{R: 0xC4, G: 0xA7, B: 0xE7, ANSI256: 183, ANSIBasic: color.FgMagenta}
	Pink     = PastelColor{R: 0xF5, G: 0xA9, B: 0xB8, ANSI256: 218, ANSIBasic: color.FgMagenta}
)

// activeProfile is the detected color profile (computed once).
var (
	activeProfile     ColorProfile
	activeProfileOnce sync.Once
)

// Profile returns the cached color profile for the current terminal.
func Profile() ColorProfile {
	activeProfileOnce.Do(func() {
		activeProfile = detectProfile()
	})
	return activeProfile
}

// SetProfile overrides the detected profile. Useful for tests.
func SetProfile(p ColorProfile) {
	activeProfileOnce.Do(func() {}) // ensure Once fires so future calls are no-ops
	activeProfile = p
}

// detectProfile inspects environment variables and TTY state.
func detectProfile() ColorProfile {
	// NO_COLOR convention (https://no-color.org/)
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return ProfileNone
	}

	// Non-TTY → no color
	if !isTTY() {
		return ProfileNone
	}

	term := os.Getenv("TERM")
	if term == "dumb" {
		return ProfileNone
	}

	// True-color detection
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	if ct == "truecolor" || ct == "24bit" {
		return ProfileTrueColor
	}

	// 256-color detection
	if strings.Contains(term, "256color") {
		return ProfileANSI256
	}

	return ProfileBasic
}

// isTTY returns true when stdout is a terminal.
func isTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// colorize applies the appropriate escape sequence for the active profile.
func colorize(c PastelColor, text string, isBold bool) string {
	p := Profile()

	boldPrefix := ""
	if isBold {
		boldPrefix = "\033[1m"
	}

	switch p {
	case ProfileTrueColor:
		return fmt.Sprintf("%s\033[38;2;%d;%d;%dm%s\033[0m", boldPrefix, c.R, c.G, c.B, text)
	case ProfileANSI256:
		return fmt.Sprintf("%s\033[38;5;%dm%s\033[0m", boldPrefix, c.ANSI256, text)
	case ProfileBasic:
		printer := color.New(c.ANSIBasic)
		if isBold {
			printer.Add(color.Bold)
		}
		return printer.Sprint(text)
	default: // ProfileNone
		return text
	}
}
