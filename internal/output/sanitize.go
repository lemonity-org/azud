package output

import (
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

// maxPendingSGR bounds how many bytes of a possible SGR sequence a
// TerminalWriter holds back while waiting for the rest of the sequence.
const maxPendingSGR = 64

// SanitizeTerminal neutralizes terminal control sequences in untrusted text,
// such as workload output or remote stderr. Complete SGR color sequences
// (ESC [ digits ; m) are removed. CRLF and lone CR become LF; newline and tab
// are kept. Every other C0 control, ESC, DEL, C1 control (U+0080-U+009F),
// invalid UTF-8 byte, Unicode bidi override or isolate, and U+2028/U+2029 is
// rendered visibly as \xNN or \uNNNN, so an injection attempt stays visible
// and is never interpreted by the terminal.
func SanitizeTerminal(value string) string {
	var sanitizer terminalSanitizer
	return string(sanitizer.sanitize(nil, []byte(value), true))
}

// IsTerminal reports whether writer is an interactive terminal.
func IsTerminal(writer io.Writer) bool {
	return isTTYWriter(writer)
}

// TerminalWriter applies SanitizeTerminal to a byte stream. Output is passed
// through as soon as it arrives; only an incomplete UTF-8 rune or SGR sequence
// at the end of a write is held back until the next write. Call Flush when the
// stream ends.
type TerminalWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	sanitizer terminalSanitizer
	buffer    []byte
}

// NewTerminalWriter returns a writer that sanitizes everything written to it
// before passing it to writer.
func NewTerminalWriter(writer io.Writer) *TerminalWriter {
	return &TerminalWriter{writer: writer}
}

// Write sanitizes p and writes the result. It reports len(p) on success so
// callers such as io.Copy do not treat the changed length as a short write.
func (w *TerminalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer = w.sanitizer.sanitize(w.buffer[:0], p, false)
	if len(w.buffer) == 0 {
		return len(p), nil
	}
	if _, err := w.writer.Write(w.buffer); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes any held-back partial sequence in its escaped form.
func (w *TerminalWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer = w.sanitizer.sanitize(w.buffer[:0], nil, true)
	if len(w.buffer) == 0 {
		return nil
	}
	_, err := w.writer.Write(w.buffer)
	return err
}

// terminalSanitizer holds the state that must survive between writes: bytes
// that may begin an SGR sequence or a multi-byte rune, and whether the last
// byte was a CR whose LF should be absorbed.
type terminalSanitizer struct {
	pending []byte
	afterCR bool
}

type sgrMatch int

const (
	sgrInvalid sgrMatch = iota
	sgrComplete
	sgrIncomplete
)

// sanitize appends the sanitized form of the pending bytes plus input to dst.
// Unless final is set, an incomplete trailing sequence is kept for the next
// call.
func (s *terminalSanitizer) sanitize(dst, input []byte, final bool) []byte {
	data := input
	if len(s.pending) > 0 {
		data = append(s.pending, input...)
		s.pending = nil
	}

	for index := 0; index < len(data); {
		char := data[index]
		if s.afterCR {
			s.afterCR = false
			if char == '\n' {
				index++
				continue
			}
		}

		switch {
		case char == '\r':
			dst = append(dst, '\n')
			s.afterCR = true
			index++
		case char == '\n' || char == '\t':
			dst = append(dst, char)
			index++
		case char == 0x1b:
			length, match := matchSGR(data[index:])
			switch {
			case match == sgrComplete:
				index += length
			case match == sgrIncomplete && !final:
				s.pending = append([]byte(nil), data[index:]...)
				return dst
			default:
				dst = appendEscapedByte(dst, char)
				index++
			}
		case char < 0x20 || char == 0x7f:
			dst = appendEscapedByte(dst, char)
			index++
		case char < utf8.RuneSelf:
			dst = append(dst, char)
			index++
		default:
			if !final && !utf8.FullRune(data[index:]) {
				s.pending = append([]byte(nil), data[index:]...)
				return dst
			}
			r, size := utf8.DecodeRune(data[index:])
			switch {
			case r == utf8.RuneError && size == 1:
				dst = appendEscapedByte(dst, char)
			case r <= 0x9f:
				dst = appendEscapedByte(dst, byte(r))
			case unsafeTerminalRune(r):
				dst = fmt.Appendf(dst, `\u%04x`, r)
			default:
				dst = append(dst, data[index:index+size]...)
			}
			index += size
		}
	}
	return dst
}

// matchSGR reports whether data, which starts with ESC, begins with a complete
// SGR sequence, could still become one, or cannot.
func matchSGR(data []byte) (int, sgrMatch) {
	if len(data) < 2 {
		return len(data), sgrIncomplete
	}
	if data[1] != '[' {
		return 0, sgrInvalid
	}
	for index := 2; index < len(data); index++ {
		char := data[index]
		switch {
		case char == 'm':
			return index + 1, sgrComplete
		case (char >= '0' && char <= '9') || char == ';':
			if index >= maxPendingSGR {
				return 0, sgrInvalid
			}
		default:
			return 0, sgrInvalid
		}
	}
	return len(data), sgrIncomplete
}

func appendEscapedByte(dst []byte, value byte) []byte {
	return fmt.Appendf(dst, `\x%02x`, value)
}

// unsafeTerminalRune reports runes that can reorder or break displayed lines:
// bidi embeddings, overrides and isolates, and the Unicode line and paragraph
// separators.
func unsafeTerminalRune(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x2028 || r == 0x2029
}
