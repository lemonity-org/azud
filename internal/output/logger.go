package output

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
)

// Logger handles formatted output for the CLI
type Logger struct {
	out     io.Writer
	err     io.Writer
	verbose bool
	mu      sync.Mutex
}

// DefaultLogger is the default logger instance
var DefaultLogger = NewLogger(os.Stdout, os.Stderr, false)

// NewLogger creates a new logger
func NewLogger(out, err io.Writer, verbose bool) *Logger {
	return &Logger{
		out:     out,
		err:     err,
		verbose: verbose,
	}
}

// SetVerbose enables or disables verbose output
func (l *Logger) SetVerbose(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbose = v
}

// Text weight attribute from fatih/color.
var faint = color.New(color.Faint).SprintFunc()

// Info prints an info message
func (l *Logger) Info(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s\n", Lavender.Sprint(SymInfo), msg)
}

// Success prints a success message
func (l *Logger) Success(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s\n", Mint.Sprint(SymSuccess), msg)
}

// Warn prints a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s\n", Peach.Sprint(SymWarn), Peach.Sprint(msg))
}

// Error prints an error message
func (l *Logger) Error(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.err, "  %s %s\n", Rose.Bold(SymError), Rose.Sprint(msg))
}

// Fatal prints an error message and exits
func (l *Logger) Fatal(format string, args ...interface{}) {
	l.Error(format, args...)
	os.Exit(1)
}

// Debug prints a debug message (only in verbose mode)
func (l *Logger) Debug(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.verbose {
		return
	}
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s\n", faint(SymDebug), faint(msg))
}

// Host prints a message prefixed with the host
func (l *Logger) Host(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s %s\n", Mauve.Sprint(SymHost), Mauve.Bold(host), msg)
}

// HostSuccess prints a success message for a host
func (l *Logger) HostSuccess(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "  %s %s %s %s\n", Mauve.Sprint(SymHost), Mauve.Bold(host), Mint.Sprint(SymSuccess), msg)
}

// HostError prints an error message for a host
func (l *Logger) HostError(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.err, "  %s %s %s %s\n", Mauve.Sprint(SymHost), Mauve.Bold(host), Rose.Bold(SymError), Rose.Sprint(msg))
}

// Step prints a step message
func (l *Logger) Step(step int, total int, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	counter := Pink.Sprint(fmt.Sprintf("[%d/%d]", step, total))
	_, _ = fmt.Fprintf(l.out, "  %s %s %s\n", Lavender.Sprint(SymInfo), counter, msg)
}

// Command prints a command being executed
func (l *Logger) Command(cmd string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, "  %s %s\n", faint(SymCommand), faint(cmd))
}

// Output prints command output
func (l *Logger) Output(output string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if output == "" {
		return
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		_, _ = fmt.Fprintf(l.out, "    %s\n", faint(line))
	}
}

// Header prints a section header
func (l *Logger) Header(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "\n  %s %s\n", Pink.Sprint(SymHeader), Pink.Bold(msg))
	_, _ = fmt.Fprintf(l.out, "    %s\n", faint(strings.Repeat("─", len(msg))))
}

// Print prints a plain message
func (l *Logger) Print(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, format, args...)
}

// Println prints a plain message with newline
func (l *Logger) Println(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, format+"\n", args...)
}

// Phase represents a single step in a deployment pipeline.
type Phase struct {
	Name     string
	Complete bool
}

// TrafficBar renders a 40-char horizontal bar showing the canary/stable traffic split.
func (l *Logger) TrafficBar(canaryPct int, canaryLabel, stableLabel string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	canaryPct = clamp(canaryPct, 0, 100)
	stablePct := 100 - canaryPct

	if Profile() == ProfileNone {
		_, _ = fmt.Fprintf(l.out, "    [%d%% %s / %d%% %s]\n", canaryPct, canaryLabel, stablePct, stableLabel)
		return
	}

	const barWidth = 40
	canaryChars := barWidth * canaryPct / 100
	stableChars := barWidth - canaryChars

	canaryBar := Mint.Sprint(strings.Repeat(SymFilled, canaryChars))
	stableBar := Lavender.Sprint(strings.Repeat(SymFilled, stableChars))

	canaryInfo := fmt.Sprintf(" %d%% %s", canaryPct, canaryLabel)
	stableInfo := fmt.Sprintf(" %d%% %s", stablePct, stableLabel)

	_, _ = fmt.Fprintf(l.out, "    %s%s%s%s\n", canaryBar, Mint.Sprint(canaryInfo), stableBar, Lavender.Sprint(stableInfo))
}

// HostPhase renders a host name followed by a phase pipeline showing completed and pending steps.
func (l *Logger) HostPhase(host string, phases []Phase) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var parts []string
	for _, p := range phases {
		if p.Complete {
			parts = append(parts, Mint.Sprint(SymSuccess)+" "+p.Name)
		} else {
			parts = append(parts, faint(SymPending)+" "+faint(p.Name))
		}
	}

	_, _ = fmt.Fprintf(l.out, "  %s %s  %s\n", Mauve.Sprint(SymHost), Mauve.Bold(host), strings.Join(parts, "  "))
}

// StatusBadge renders a key-value line with the status value colorized by state.
// Known statuses map to specific colors: "running" (Mint), "deploying" (Peach),
// "promoting" (SkyBlue), "rolling_back" (Rose). Any other value uses Lavender.
func (l *Logger) StatusBadge(label, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var colored string
	switch status {
	case "running":
		colored = Mint.Bold(status)
	case "deploying":
		colored = Peach.Bold(status)
	case "promoting":
		colored = SkyBlue.Bold(status)
	case "rolling_back":
		colored = Rose.Bold(status)
	default:
		colored = Lavender.Sprint(status)
	}

	_, _ = fmt.Fprintf(l.out, "  %s %-16s %s\n", Lavender.Sprint(SymInfo), label, colored)
}

// clamp restricts v to the range [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Timer tracks and logs operation duration
type Timer struct {
	name  string
	start time.Time
	log   *Logger
}

// NewTimer creates a new timer
func (l *Logger) NewTimer(name string) *Timer {
	return &Timer{
		name:  name,
		start: time.Now(),
		log:   l,
	}
}

// Stop stops the timer and logs the duration
func (t *Timer) Stop() time.Duration {
	duration := time.Since(t.start)
	t.log.Debug("%s completed in %s", t.name, duration.Round(time.Millisecond))
	return duration
}

// Progress represents a progress indicator
type Progress struct {
	total   int
	current int
	name    string
	log     *Logger
	mu      sync.Mutex
}

// NewProgress creates a new progress indicator
func (l *Logger) NewProgress(name string, total int) *Progress {
	return &Progress{
		total: total,
		name:  name,
		log:   l,
	}
}

// Increment increments the progress
func (p *Progress) Increment(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current++
	p.log.Step(p.current, p.total, "%s", msg)
}

// Done marks the progress as complete
func (p *Progress) Done() {
	p.log.Success("%s complete", p.name)
}

// Table prints data in a table format
func (l *Logger) Table(headers []string, rows [][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Calculate column widths
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Print header with indent and lavender bold
	for i, h := range headers {
		if i == 0 {
			_, _ = fmt.Fprintf(l.out, "    ")
		}
		_, _ = fmt.Fprintf(l.out, "%-*s  ", widths[i], Lavender.Bold(h))
	}
	_, _ = fmt.Fprintln(l.out)

	// Print separator with box-drawing characters
	for i := range headers {
		if i == 0 {
			_, _ = fmt.Fprintf(l.out, "    ")
		}
		_, _ = fmt.Fprintf(l.out, "%s  ", faint(strings.Repeat("─", widths[i])))
	}
	_, _ = fmt.Fprintln(l.out)

	// Print rows
	for _, row := range rows {
		for i, cell := range row {
			if i == 0 {
				_, _ = fmt.Fprintf(l.out, "    ")
			}
			if i < len(widths) {
				_, _ = fmt.Fprintf(l.out, "%-*s  ", widths[i], cell)
			}
		}
		_, _ = fmt.Fprintln(l.out)
	}
}

// Package-level functions for convenience

// Info prints an info message using the default logger
func Info(format string, args ...interface{}) {
	DefaultLogger.Info(format, args...)
}

// Success prints a success message using the default logger
func Success(format string, args ...interface{}) {
	DefaultLogger.Success(format, args...)
}

// Warn prints a warning message using the default logger
func Warn(format string, args ...interface{}) {
	DefaultLogger.Warn(format, args...)
}

// Error prints an error message using the default logger
func Error(format string, args ...interface{}) {
	DefaultLogger.Error(format, args...)
}

// Debug prints a debug message using the default logger
func Debug(format string, args ...interface{}) {
	DefaultLogger.Debug(format, args...)
}

// SetVerbose sets verbose mode on the default logger
func SetVerbose(v bool) {
	DefaultLogger.SetVerbose(v)
}

// Println prints a line using the default logger
func Println(format string, args ...interface{}) {
	DefaultLogger.Println(format, args...)
}

// TrafficBar renders a traffic bar using the default logger
func TrafficBar(canaryPct int, canaryLabel, stableLabel string) {
	DefaultLogger.TrafficBar(canaryPct, canaryLabel, stableLabel)
}

// HostPhase renders a host phase pipeline using the default logger
func HostPhase(host string, phases []Phase) {
	DefaultLogger.HostPhase(host, phases)
}

// StatusBadge renders a status badge using the default logger
func StatusBadge(label, status string) {
	DefaultLogger.StatusBadge(label, status)
}
