package output

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

const (
	recordLabelWidth = 5
	recordIndent     = "  "
	recordGutter     = "  "
	defaultRuleWidth = 56
	trafficBarWidth  = 32
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Logger handles formatted output for the CLI.
type Logger struct {
	out        io.Writer
	err        io.Writer
	verbose    bool
	width      int
	outStarted bool
	mu         sync.Mutex
}

// DefaultLogger is the default logger instance.
var DefaultLogger = NewLogger(os.Stdout, os.Stderr, false)

// NewLogger creates a logger. ANSI and Unicode capabilities are detected for
// each destination writer; buffers, files, pipes, and CI logs stay plain.
func NewLogger(out, err io.Writer, verbose bool) *Logger {
	return &Logger{
		out:     out,
		err:     err,
		verbose: verbose,
	}
}

// SetVerbose enables or disables verbose output.
func (l *Logger) SetVerbose(verbose bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbose = verbose
}

// SetWidth overrides automatic terminal-width detection. Zero restores
// automatic sizing. It is useful for embedded and test renderers.
func (l *Logger) SetWidth(columns int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if columns < 0 {
		columns = 0
	}
	l.width = columns
}

// Info prints an informational record.
func (l *Logger) Info(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("INFO", Blue, fmt.Sprintf(format, args...))
}

// Success prints a successful-state record.
func (l *Logger) Success(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("OK", Green, fmt.Sprintf(format, args...))
}

// Warn prints a warning record.
func (l *Logger) Warn(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("WARN", Yellow, fmt.Sprintf(format, args...))
}

// Error prints an error record.
func (l *Logger) Error(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeRecord(l.err, "ERROR", Red, fmt.Sprintf(format, args...))
}

// Fatal prints an error message and exits.
func (l *Logger) Fatal(format string, args ...interface{}) {
	l.Error(format, args...)
	os.Exit(1)
}

// Debug prints a debug record when verbose mode is enabled.
func (l *Logger) Debug(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.verbose {
		return
	}
	l.writeOutRecord("DEBUG", Gray, fmt.Sprintf(format, args...))
}

// Host prints an in-progress host record.
func (l *Logger) Host(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("HOST", Blue, hostMessage(host, fmt.Sprintf(format, args...)))
}

// HostSuccess prints a successful host record.
func (l *Logger) HostSuccess(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("OK", Green, hostMessage(host, fmt.Sprintf(format, args...)))
}

// HostError prints a failed host record.
func (l *Logger) HostError(host, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeRecord(l.err, "ERROR", Red, hostMessage(host, fmt.Sprintf(format, args...)))
}

// Step prints a numbered operation record.
func (l *Logger) Step(step int, total int, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	message := fmt.Sprintf("%d/%d  %s", step, total, fmt.Sprintf(format, args...))
	l.writeOutRecord("STEP", Blue, message)
}

// Command prints a command being executed.
func (l *Logger) Command(command string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutRecord("CMD", Gray, command)
}

// Output prints captured child-command output beneath the preceding record.
// Leading whitespace and meaningful blank lines are preserved.
func (l *Logger) Output(output string) {
	if output == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutput(l.out, output)
	l.outStarted = true
}

// OutputError prints captured child-command diagnostics on stderr.
func (l *Logger) OutputError(output string) {
	if output == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOutput(l.err, output)
}

func (l *Logger) writeOutput(writer io.Writer, output string) {
	if output == "" {
		return
	}

	normalized := normalizeLines(output)
	normalized = strings.TrimSuffix(normalized, "\n")
	rail := "|"
	if supportsUnicode(writer) {
		rail = SymRail
	}
	rail = styleForWriter(writer, Gray, rail, false)

	for _, line := range strings.Split(normalized, "\n") {
		_, _ = fmt.Fprintf(writer, "%s%s%s%s %s\n", recordIndent, strings.Repeat(" ", recordLabelWidth), recordGutter, rail, line)
	}
}

// Header prints a compact ruled section heading.
func (l *Logger) Header(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.outStarted {
		_, _ = fmt.Fprintln(l.out)
	}

	marker := "#"
	rule := "-"
	if supportsUnicode(l.out) {
		marker = SymHeader
		rule = "─"
	}

	title := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.out, "%s%s %s\n", recordIndent, styleForWriter(l.out, Red, marker, true), title)
	_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, styleForWriter(l.out, Gray, strings.Repeat(rule, l.ruleWidth()), false))
	l.outStarted = true
}

// Print writes raw output. Use it for machine-readable command surfaces.
func (l *Logger) Print(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, format, args...)
	l.outStarted = true
}

// Println writes a raw line. Use it for machine-readable command surfaces.
func (l *Logger) Println(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, format+"\n", args...)
	l.outStarted = true
}

// Phase represents a single step in a deployment pipeline.
type Phase struct {
	Name     string
	Complete bool
}

// TrafficBar renders the canary/stable traffic split as a fixed technical
// gauge plus explicit written percentages.
func (l *Logger) TrafficBar(canaryPct int, canaryLabel, stableLabel string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	canaryPct = clamp(canaryPct, 0, 100)
	stablePct := 100 - canaryPct
	barWidth := trafficBarWidth
	if columns := l.outputWidth(); columns > 0 {
		barWidth = columns - 19
		if barWidth < 8 {
			l.writeOutRecord("SPLIT", Blue, fmt.Sprintf("%03d/%03d", canaryPct, stablePct))
			l.writeTrafficDetails(canaryPct, canaryLabel, stablePct, stableLabel)
			return
		}
		if barWidth > trafficBarWidth {
			barWidth = trafficBarWidth
		}
	}
	canaryCells := barWidth * canaryPct / 100
	stableCells := barWidth - canaryCells

	canaryRune := "#"
	stableRune := "-"
	if supportsUnicode(l.out) {
		canaryRune = SymFilled
		stableRune = SymEmpty
	}

	canaryBar := styleForWriter(l.out, Blue, strings.Repeat(canaryRune, canaryCells), false)
	stableBar := styleForWriter(l.out, Green, strings.Repeat(stableRune, stableCells), false)
	bar := "[" + canaryBar + stableBar + "]"
	l.writeOutRecord("SPLIT", Blue, fmt.Sprintf("%s %03d/%03d", bar, canaryPct, stablePct))
	l.writeTrafficDetails(canaryPct, canaryLabel, stablePct, stableLabel)
}

func (l *Logger) writeTrafficDetails(canaryPct int, canaryLabel string, stablePct int, stableLabel string) {
	details := fmt.Sprintf("%d%% %s / %d%% %s", canaryPct, canaryLabel, stablePct, stableLabel)
	width := l.outputWidth()
	if width == 0 {
		l.writeContinuation(l.out, details)
		return
	}
	for _, line := range wrapWordsDisplay(details, width-9) {
		l.writeContinuation(l.out, line)
	}
}

// HostPhase renders a host followed by explicit complete and pending phases.
func (l *Logger) HostPhase(host string, phases []Phase) {
	l.mu.Lock()
	defer l.mu.Unlock()

	complete, pending := "[x]", "[ ]"
	if supportsUnicode(l.out) {
		complete, pending = SymHeader, SymPending
	}

	parts := make([]string, 0, len(phases))
	for _, phase := range phases {
		if phase.Complete {
			parts = append(parts, styleForWriter(l.out, Green, complete, false)+" "+phase.Name)
		} else {
			parts = append(parts, styleForWriter(l.out, Gray, pending, false)+" "+phase.Name)
		}
	}

	message := host
	if len(parts) > 0 {
		message += " / " + strings.Join(parts, "  ")
	}
	l.writeOutRecord("HOST", Blue, message)
}

// StatusBadge renders a key-value record with an explicit uppercase state.
func (l *Logger) StatusBadge(label, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	tone := Blue
	switch strings.ToLower(status) {
	case "running":
		tone = Green
	case "deploying":
		tone = Yellow
	case "promoting":
		tone = Blue
	case "rolling_back":
		tone = Red
	}

	chip := styleForWriter(l.out, tone, "["+strings.ToUpper(status)+"]", true)
	l.writeOutRecord("STATE", tone, padRight(label, 16)+" "+chip)
}

// Timer tracks and logs operation duration.
type Timer struct {
	name  string
	start time.Time
	log   *Logger
}

// NewTimer creates a new timer.
func (l *Logger) NewTimer(name string) *Timer {
	return &Timer{
		name:  name,
		start: time.Now(),
		log:   l,
	}
}

// Stop stops the timer and logs the duration.
func (t *Timer) Stop() time.Duration {
	duration := time.Since(t.start)
	t.log.Debug("%s completed in %s", t.name, duration.Round(time.Millisecond))
	return duration
}

// Progress represents a progress indicator.
type Progress struct {
	total   int
	current int
	name    string
	log     *Logger
	mu      sync.Mutex
}

// NewProgress creates a new progress indicator.
func (l *Logger) NewProgress(name string, total int) *Progress {
	return &Progress{
		total: total,
		name:  name,
		log:   l,
	}
}

// Increment increments the progress.
func (p *Progress) Increment(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current++
	p.log.Step(p.current, p.total, "%s", message)
}

// Done marks the progress as complete.
func (p *Progress) Done() {
	p.log.Success("%s complete", p.name)
}

// Table prints dense tabular data. Interactive narrow terminals reflow rows
// into labeled records; non-TTY output remains deterministic and columnar.
func (l *Logger) Table(headers []string, rows [][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(headers) == 0 {
		return
	}

	headers, rows = sanitizeTable(headers, rows)
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = displayWidth(header)
	}
	for _, row := range rows {
		for index := 0; index < len(headers) && index < len(row); index++ {
			if width := displayWidth(row[index]); width > widths[index] {
				widths[index] = width
			}
		}
	}

	tableWidth := len(recordIndent) + sum(widths) + 2*(len(widths)-1)
	if columns := l.outputWidth(); columns > 0 && tableWidth > columns {
		if len(rows) == 0 {
			l.writeEmptyRecordTable(headers)
		} else {
			l.writeRecordTable(headers, rows)
		}
		l.outStarted = true
		return
	}

	headerCells := make([]string, len(headers))
	ruleCells := make([]string, len(headers))
	for index, header := range headers {
		if index < len(headers)-1 {
			header = padRight(header, widths[index])
		}
		headerCells[index] = styleForWriter(l.out, Blue, header, true)
		ruleCells[index] = styleForWriter(l.out, Gray, strings.Repeat("-", widths[index]), false)
	}
	_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, strings.Join(headerCells, "  "))
	_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, strings.Join(ruleCells, "  "))

	for _, row := range rows {
		cells := make([]string, len(headers))
		for index := range headers {
			cell := ""
			if index < len(row) {
				cell = row[index]
			}
			cells[index] = padRight(cell, widths[index])
		}
		_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	l.outStarted = true
}

func (l *Logger) writeRecordTable(headers []string, rows [][]string) {
	for rowIndex, row := range rows {
		if rowIndex > 0 {
			_, _ = fmt.Fprintln(l.out)
		}
		l.writeRecord(l.out, "REC", Blue, fmt.Sprintf("%02d/%02d", rowIndex+1, len(rows)))
		for columnIndex, header := range headers {
			value := ""
			if columnIndex < len(row) {
				value = row[columnIndex]
			}
			label := styleForWriter(l.out, Blue, header, true)
			_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, label)
			if value == "" {
				_, _ = fmt.Fprintln(l.out)
			} else {
				_, _ = fmt.Fprintf(l.out, "    %s\n", value)
			}
		}
	}
}

func (l *Logger) writeEmptyRecordTable(headers []string) {
	l.writeRecord(l.out, "REC", Blue, "0")
	for _, header := range headers {
		label := styleForWriter(l.out, Blue, header, true)
		_, _ = fmt.Fprintf(l.out, "%s%s\n", recordIndent, label)
	}
}

func (l *Logger) writeOutRecord(label string, tone PastelColor, message string) {
	l.writeRecord(l.out, label, tone, message)
	l.outStarted = true
}

func (l *Logger) writeRecord(writer io.Writer, label string, tone PastelColor, message string) {
	label = padRight(label, recordLabelWidth)
	label = styleForWriter(writer, tone, label, true)
	prefix := recordIndent + label + recordGutter

	lines := strings.Split(normalizeLines(message), "\n")
	for index, line := range lines {
		if index == 0 {
			if line == "" {
				_, _ = fmt.Fprintln(writer, strings.TrimRight(prefix, " "))
			} else {
				_, _ = fmt.Fprintln(writer, prefix+line)
			}
			continue
		}
		l.writeContinuation(writer, line)
	}
}

func (l *Logger) writeContinuation(writer io.Writer, message string) {
	_, _ = fmt.Fprintf(writer, "%s%s%s%s\n", recordIndent, strings.Repeat(" ", recordLabelWidth), recordGutter, message)
}

func (l *Logger) ruleWidth() int {
	width := defaultRuleWidth
	if columns := l.outputWidth(); columns > 0 && columns-len(recordIndent) < width {
		width = columns - len(recordIndent)
		if width < 1 {
			width = 1
		}
	}
	return width
}

func (l *Logger) outputWidth() int {
	if l.width > 0 {
		return l.width
	}
	file, ok := l.out.(*os.File)
	if !ok || !isTTYWriter(file) {
		return 0
	}
	columns, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		fallback, parseErr := strconv.Atoi(os.Getenv("COLUMNS"))
		if parseErr != nil || fallback <= 0 {
			return 0
		}
		columns = fallback
	}
	return columns
}

func hostMessage(host, message string) string {
	if message == "" {
		return host
	}
	return host + " / " + message
}

func normalizeLines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func displayWidth(value string) int {
	value = ansiPattern.ReplaceAllString(value, "")
	return uniseg.StringWidth(value)
}

func padRight(value string, width int) string {
	padding := width - displayWidth(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

func wrapWordsDisplay(value string, width int) []string {
	if width < 1 {
		width = 1
	}

	var lines []string
	for _, sourceLine := range strings.Split(normalizeLines(value), "\n") {
		words := strings.Fields(sourceLine)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}

		current := words[0]
		for _, word := range words[1:] {
			if displayWidth(current)+1+displayWidth(word) <= width {
				current += " " + word
				continue
			}
			lines = append(lines, current)
			current = word
		}
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func sanitizeTable(headers []string, rows [][]string) ([]string, [][]string) {
	cleanHeaders := make([]string, len(headers))
	for index, header := range headers {
		cleanHeaders[index] = sanitizeTableCell(header)
	}
	cleanRows := make([][]string, len(rows))
	for rowIndex, row := range rows {
		cleanRows[rowIndex] = make([]string, len(row))
		for columnIndex, cell := range row {
			cleanRows[rowIndex][columnIndex] = sanitizeTableCell(cell)
		}
	}
	return cleanHeaders, cleanRows
}

func sanitizeTableCell(value string) string {
	value = ansiPattern.ReplaceAllString(normalizeLines(value), "")
	var clean strings.Builder
	for _, character := range value {
		switch {
		case character == '\n':
			clean.WriteString(`\n`)
		case character == '\t':
			clean.WriteString("    ")
		case character < 0x20 || (character >= 0x7f && character <= 0x9f):
			fmt.Fprintf(&clean, `\x%02x`, character)
		default:
			clean.WriteRune(character)
		}
	}
	return clean.String()
}

func sum(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

// clamp restricts value to the inclusive range [low, high].
func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Package-level functions for convenience.

func Info(format string, args ...interface{}) {
	DefaultLogger.Info(format, args...)
}

func Success(format string, args ...interface{}) {
	DefaultLogger.Success(format, args...)
}

func Warn(format string, args ...interface{}) {
	DefaultLogger.Warn(format, args...)
}

func Error(format string, args ...interface{}) {
	DefaultLogger.Error(format, args...)
}

func Debug(format string, args ...interface{}) {
	DefaultLogger.Debug(format, args...)
}

func SetVerbose(verbose bool) {
	DefaultLogger.SetVerbose(verbose)
}

func Println(format string, args ...interface{}) {
	DefaultLogger.Println(format, args...)
}

func TrafficBar(canaryPct int, canaryLabel, stableLabel string) {
	DefaultLogger.TrafficBar(canaryPct, canaryLabel, stableLabel)
}

func HostPhase(host string, phases []Phase) {
	DefaultLogger.HostPhase(host, phases)
}

func StatusBadge(label, status string) {
	DefaultLogger.StatusBadge(label, status)
}
