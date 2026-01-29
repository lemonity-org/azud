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

// Colors
var (
	cyan    = color.New(color.FgCyan).SprintFunc()
	green   = color.New(color.FgGreen).SprintFunc()
	yellow  = color.New(color.FgYellow).SprintFunc()
	red     = color.New(color.FgRed).SprintFunc()
	bold    = color.New(color.Bold).SprintFunc()
	faint   = color.New(color.Faint).SprintFunc()
	magenta = color.New(color.FgMagenta).SprintFunc()
)

// Info prints an info message
func (l *Logger) Info(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s\n", cyan("INFO"), msg)
}

// Success prints a success message
func (l *Logger) Success(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s\n", green("OK"), msg)
}

// Warn prints a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s\n", yellow("WARN"), msg)
}

// Error prints an error message
func (l *Logger) Error(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.err, "%s %s\n", red("ERROR"), msg)
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
	fmt.Fprintf(l.out, "%s %s\n", faint("DEBUG"), faint(msg))
}

// Host prints a message prefixed with the host
func (l *Logger) Host(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s\n", magenta(fmt.Sprintf("[%s]", host)), msg)
}

// HostSuccess prints a success message for a host
func (l *Logger) HostSuccess(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s %s\n", magenta(fmt.Sprintf("[%s]", host)), green("OK"), msg)
}

// HostError prints an error message for a host
func (l *Logger) HostError(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.err, "%s %s %s\n", magenta(fmt.Sprintf("[%s]", host)), red("ERROR"), msg)
}

// Step prints a step message
func (l *Logger) Step(step int, total int, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "%s %s\n", cyan(fmt.Sprintf("[%d/%d]", step, total)), msg)
}

// Command prints a command being executed
func (l *Logger) Command(cmd string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s %s\n", faint("$"), faint(cmd))
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
		fmt.Fprintf(l.out, "  %s\n", line)
	}
}

// Header prints a section header
func (l *Logger) Header(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.out, "\n%s\n", bold(msg))
	fmt.Fprintf(l.out, "%s\n", strings.Repeat("-", len(msg)))
}

// Print prints a plain message
func (l *Logger) Print(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, format, args...)
}

// Println prints a plain message with newline
func (l *Logger) Println(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, format+"\n", args...)
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

	// Print header
	for i, h := range headers {
		fmt.Fprintf(l.out, "%-*s  ", widths[i], bold(h))
	}
	fmt.Fprintln(l.out)

	// Print separator
	for i := range headers {
		fmt.Fprintf(l.out, "%s  ", strings.Repeat("-", widths[i]))
	}
	fmt.Fprintln(l.out)

	// Print rows
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				fmt.Fprintf(l.out, "%-*s  ", widths[i], cell)
			}
		}
		fmt.Fprintln(l.out)
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
