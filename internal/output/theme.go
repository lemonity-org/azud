package output

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"
)

// ColorProfile represents the level of color support available.
type ColorProfile int

const (
	ProfileNone      ColorProfile = iota // No ANSI styling.
	ProfileBasic                         // 16 basic ANSI colors.
	ProfileANSI256                       // 256-color palette.
	ProfileTrueColor                     // 24-bit RGB true color.
)

// Symbols retained for compatibility with callers that build custom output.
// Structured logger output selects an ASCII fallback when Unicode is unsafe.
const (
	SymInfo    = "●"
	SymSuccess = "✓"
	SymWarn    = "▲"
	SymError   = "×"
	SymDebug   = "·"
	SymCommand = "›"
	SymHeader  = "■"
	SymHost    = "◆"
	SymLine    = "───"
	SymFilled  = "█"
	SymEmpty   = "░"
	SymPending = "□"
	SymRail    = "│"
)

// PastelColor is retained as the public color value type for source
// compatibility. The active palette is semantic rather than pastel: color
// classifies state and never carries meaning alone.
type PastelColor struct {
	R, G, B   uint8
	ANSI256   uint8
	ANSIBasic uint8
}

// Sprint returns text colored according to stdout's active profile.
func (c PastelColor) Sprint(text string) string {
	return c.render(text, false, Profile())
}

// Bold returns bold, colored text according to stdout's active profile.
func (c PastelColor) Bold(text string) string {
	return c.render(text, true, Profile())
}

func (c PastelColor) render(text string, bold bool, profile ColorProfile) string {
	if profile == ProfileNone || text == "" {
		return text
	}

	weight := ""
	if bold {
		weight = "1;"
	}

	switch profile {
	case ProfileTrueColor:
		return fmt.Sprintf("\x1b[%s38;2;%d;%d;%dm%s\x1b[0m", weight, c.R, c.G, c.B, text)
	case ProfileANSI256:
		return fmt.Sprintf("\x1b[%s38;5;%dm%s\x1b[0m", weight, c.ANSI256, text)
	case ProfileBasic:
		return fmt.Sprintf("\x1b[%s%dm%s\x1b[0m", weight, c.ANSIBasic, text)
	default:
		return text
	}
}

// Functional palette. Body copy inherits the terminal foreground so it stays
// legible on both light and dark terminal themes.
var (
	Blue   = PastelColor{R: 0x00, G: 0x2D, B: 0xCE, ANSI256: 20, ANSIBasic: 34}
	Green  = PastelColor{R: 0x00, G: 0x79, B: 0x4C, ANSI256: 29, ANSIBasic: 32}
	Yellow = PastelColor{R: 0xFF, G: 0xB7, B: 0x00, ANSI256: 214, ANSIBasic: 33}
	Red    = PastelColor{R: 0xFF, G: 0x41, B: 0x36, ANSI256: 203, ANSIBasic: 31}
	Gray   = PastelColor{R: 0x88, G: 0x88, B: 0x88, ANSI256: 102, ANSIBasic: 90}

	// Legacy palette aliases. They now map to functional state colors.
	Lavender = Blue
	Mint     = Green
	Peach    = Yellow
	Rose     = Red
	SkyBlue  = Blue
	Mauve    = Blue
	Pink     = Red
)

var profileOverride struct {
	sync.RWMutex
	value *ColorProfile
}

// Profile returns stdout's current color profile.
func Profile() ColorProfile {
	if profile, ok := overriddenProfile(); ok {
		return profile
	}
	return detectProfile()
}

// SetProfile overrides automatic detection. It is intended for tests and
// embedders that have already negotiated terminal capabilities.
func SetProfile(profile ColorProfile) {
	profileOverride.Lock()
	defer profileOverride.Unlock()
	value := profile
	profileOverride.value = &value
}

// ResetProfile restores automatic terminal detection.
func ResetProfile() {
	profileOverride.Lock()
	defer profileOverride.Unlock()
	profileOverride.value = nil
}

func overriddenProfile() (ColorProfile, bool) {
	profileOverride.RLock()
	defer profileOverride.RUnlock()
	if profileOverride.value == nil {
		return ProfileNone, false
	}
	return *profileOverride.value, true
}

// detectProfile inspects the environment and stdout's terminal capability.
func detectProfile() ColorProfile {
	return detectProfileForWriter(os.Stdout)
}

func detectProfileForWriter(writer io.Writer) ColorProfile {
	if profile, ok := overriddenProfile(); ok {
		return profile
	}
	return detectProfileForCapabilities(isTTYWriter(writer) && supportsANSIWriter(writer), os.LookupEnv)
}

func detectProfileForCapabilities(tty bool, lookup func(string) (string, bool)) ColorProfile {
	if _, ok := lookup("NO_COLOR"); ok {
		return ProfileNone
	}
	if value, _ := lookup("CLICOLOR"); value == "0" {
		return ProfileNone
	}
	if !tty {
		return ProfileNone
	}

	termValue, _ := lookup("TERM")
	term := strings.ToLower(termValue)
	if term == "dumb" {
		return ProfileNone
	}

	colorTermValue, _ := lookup("COLORTERM")
	colorTerm := strings.ToLower(colorTermValue)
	if colorTerm == "truecolor" || colorTerm == "24bit" {
		return ProfileTrueColor
	}
	if strings.Contains(term, "256color") {
		return ProfileANSI256
	}
	return ProfileBasic
}

func isTTYWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd())
}

func supportsANSIWriter(writer io.Writer) bool {
	if runtime.GOOS != "windows" {
		return true
	}
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	if isatty.IsCygwinTerminal(file.Fd()) {
		return true
	}
	return os.Getenv("WT_SESSION") != "" ||
		os.Getenv("ANSICON") != "" ||
		strings.EqualFold(os.Getenv("ConEmuANSI"), "ON") ||
		strings.Contains(strings.ToLower(os.Getenv("TERM")), "xterm")
}

func supportsUnicode(writer io.Writer) bool {
	locale := os.Getenv("LC_ALL")
	if locale == "" {
		locale = os.Getenv("LC_CTYPE")
	}
	if locale == "" {
		locale = os.Getenv("LANG")
	}
	return supportsUnicodeForCapabilities(
		isTTYWriter(writer),
		runtime.GOOS,
		os.Getenv("TERM"),
		locale,
		supportsANSIWriter(writer),
	)
}

func supportsUnicodeForCapabilities(tty bool, platform, termName, locale string, windowsANSI bool) bool {
	if !tty || strings.EqualFold(termName, "dumb") {
		return false
	}
	if platform == "windows" {
		return windowsANSI
	}
	normalized := strings.ToLower(locale)
	return strings.Contains(normalized, "utf-8") || strings.Contains(normalized, "utf8")
}

func styleForWriter(writer io.Writer, color PastelColor, text string, bold bool) string {
	profile := detectProfileForWriter(writer)
	if profile != ProfileNone {
		// Text accents use the terminal's semantic ANSI palette even when
		// richer color is available. That lets light, dark, and high-contrast
		// themes choose legible values while preserving Azud's state mapping.
		profile = ProfileBasic
	}
	return color.render(text, bold, profile)
}
