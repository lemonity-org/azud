package output

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSanitizeTerminal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain text", input: "ready on :3000\n", want: "ready on :3000\n"},
		{name: "osc 52 with bel", input: "\x1b]52;c;ZXZpbA==\x07ok", want: `\x1b]52;c;ZXZpbA==\x07ok`},
		{name: "osc with string terminator", input: "\x1b]0;title\x1b\\ok", want: `\x1b]0;title\x1b\ok`},
		{name: "clear screen", input: "\x1b[2Jok", want: `\x1b[2Jok`},
		{name: "cursor up", input: "\x1b[1Aok", want: `\x1b[1Aok`},
		{name: "cursor home", input: "\x1b[Hok", want: `\x1b[Hok`},
		{name: "device status query", input: "\x1b[6n", want: `\x1b[6n`},
		{name: "sgr removed", input: "\x1b[31mFAILED\x1b[0m done", want: "FAILED done"},
		{name: "sgr without parameters", input: "\x1b[mreset", want: "reset"},
		{name: "conceal removed", input: "\x1b[8mhidden\x1b[28m", want: "hidden"},
		{name: "c0 controls", input: "a\bb\ac\x00d\x7f", want: `a\x08b\x07c\x00d\x7f`},
		{name: "lone escape", input: "end\x1b", want: `end\x1b`},
		{name: "c1 csi rune", input: "a\u009b2Jb", want: `a\x9b2Jb`},
		{name: "c1 next line rune", input: "a\u0085b", want: `a\x85b`},
		{name: "raw 8-bit csi byte", input: "a\x9b2Jb", want: `a\x9b2Jb`},
		{name: "truncated rune", input: "a\xe2\x82", want: `a\xe2\x82`},
		{name: "crlf", input: "a\r\nb\r\n", want: "a\nb\n"},
		{name: "lone cr", input: "10%\r20%\rdone", want: "10%\n20%\ndone"},
		{name: "cr before crlf", input: "a\r\r\nb", want: "a\n\nb"},
		{name: "tab and newline kept", input: "a\tb\nc", want: "a\tb\nc"},
		{name: "unicode kept", input: "café 日本語 🙂", want: "café 日本語 🙂"},
		{name: "bidi override", input: "user\u202eexe.txt", want: `user\u202eexe.txt`},
		{name: "bidi isolate", input: "a\u2066b\u2069", want: `a\u2066b\u2069`},
		{name: "line separator", input: "a\u2028b", want: `a\u2028b`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeTerminal(tt.input); got != tt.want {
				t.Fatalf("SanitizeTerminal(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTerminalWriterMatchesSanitizeAtEverySplit(t *testing.T) {
	inputs := []string{
		"\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mFORGED\x1b[0m\n",
		"café\r\n日本語\r🙂\x1b[1;32mok\x1b[0m\u009b\n",
		"partial \x1b[12",
	}
	for _, input := range inputs {
		want := SanitizeTerminal(input)
		for split := 0; split <= len(input); split++ {
			var out bytes.Buffer
			writer := NewTerminalWriter(&out)
			for _, chunk := range []string{input[:split], input[split:]} {
				n, err := writer.Write([]byte(chunk))
				if err != nil || n != len(chunk) {
					t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
				}
			}
			if err := writer.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if out.String() != want {
				t.Fatalf("split %d of %q = %q, want %q", split, input, out.String(), want)
			}
		}
	}
}

func TestTerminalWriterSequenceSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	writer := NewTerminalWriter(&out)
	_, _ = writer.Write([]byte("\x1b"))
	_, _ = writer.Write([]byte("]52;c;eA==\x07\n"))
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("split OSC reached the output raw: %q", out.String())
	}
	if out.String() != `\x1b]52;c;eA==\x07`+"\n" {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestTerminalWriterPassesCompleteLinesImmediately(t *testing.T) {
	var out bytes.Buffer
	writer := NewTerminalWriter(&out)
	_, _ = writer.Write([]byte("progress 10%"))
	if out.String() != "progress 10%" {
		t.Fatalf("partial line was delayed: %q", out.String())
	}
	_, _ = writer.Write([]byte("\r"))
	if out.String() != "progress 10%\n" {
		t.Fatalf("carriage return was delayed: %q", out.String())
	}
	_, _ = writer.Write([]byte("\ndone"))
	if out.String() != "progress 10%\ndone" {
		t.Fatalf("CRLF split across writes produced %q", out.String())
	}
}

func TestTerminalWriterFlushEmitsPendingSequence(t *testing.T) {
	var out bytes.Buffer
	writer := NewTerminalWriter(&out)
	_, _ = writer.Write([]byte("tail\x1b[3"))
	if out.String() != "tail" {
		t.Fatalf("incomplete SGR was not held back: %q", out.String())
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if out.String() != `tail\x1b[3` {
		t.Fatalf("Flush output = %q", out.String())
	}
}

func TestTerminalWriterCapsPendingSGR(t *testing.T) {
	var out bytes.Buffer
	writer := NewTerminalWriter(&out)
	long := "\x1b[" + strings.Repeat("1;", maxPendingSGR)
	_, _ = writer.Write([]byte(long))
	if !strings.HasPrefix(out.String(), `\x1b[1;`) {
		t.Fatalf("oversized SGR prefix was held back: %q", out.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestTerminalWriterReportsDownstreamErrors(t *testing.T) {
	writer := NewTerminalWriter(failingWriter{})
	if _, err := writer.Write([]byte("x\n")); err == nil {
		t.Fatal("expected downstream write error")
	}
}
