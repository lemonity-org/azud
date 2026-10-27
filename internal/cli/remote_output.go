package cli

import (
	"io"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/ssh"
)

// remoteStreamKind selects how untrusted remote output is neutralized before
// it reaches the operator's terminal.
type remoteStreamKind int

const (
	// remoteLogStream is container log text. It is sanitized on every
	// destination, including pipes and files, unless --raw is set.
	remoteLogStream remoteStreamKind = iota
	// remoteExecStream is non-interactive exec output, which may be binary
	// (dumps, archives). It is sanitized only when written to a terminal.
	remoteExecStream
)

const rawOutputFlagUsage = "Print remote output byte-for-byte without neutralizing terminal control sequences"

// isTerminalWriter is a test seam for terminal detection.
var isTerminalWriter = output.IsTerminal

// remoteWriter wraps w in an output.TerminalWriter unless raw is set, or kind
// is remoteExecStream and w is not a terminal. The returned function flushes
// any held-back partial sequence and must be called when the stream ends.
// Interactive sessions (exec -i/-t) must not use it: they need the full
// terminal protocol.
func remoteWriter(w io.Writer, kind remoteStreamKind, raw bool) (io.Writer, func()) {
	if raw || (kind == remoteExecStream && !isTerminalWriter(w)) {
		return w, func() {}
	}
	writer := output.NewTerminalWriter(w)
	return writer, func() { _ = writer.Flush() }
}

// remoteStreams wraps stdout and stderr for a streamed remote command. The
// returned function flushes both and must be called when the stream ends.
func remoteStreams(stdout, stderr io.Writer, kind remoteStreamKind, raw bool) (io.Writer, io.Writer, func()) {
	safeStdout, flushStdout := remoteWriter(stdout, kind, raw)
	safeStderr, flushStderr := remoteWriter(stderr, kind, raw)
	return safeStdout, safeStderr, func() {
		flushStdout()
		flushStderr()
	}
}

// printRemoteResult writes a buffered remote result's stdout and stderr.
// Write errors are ignored, as with direct printing.
func printRemoteResult(stdout, stderr io.Writer, result *ssh.Result, kind remoteStreamKind, raw bool) {
	safeStdout, safeStderr, flush := remoteStreams(stdout, stderr, kind, raw)
	_, _ = io.WriteString(safeStdout, result.Stdout)
	_, _ = io.WriteString(safeStderr, result.Stderr)
	flush()
}
