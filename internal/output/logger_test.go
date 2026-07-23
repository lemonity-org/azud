package output

import (
	"bytes"
	"strings"
	"testing"
)

func newTestLogger(verbose ...bool) (*Logger, *bytes.Buffer, *bytes.Buffer) {
	var outBuffer, errBuffer bytes.Buffer
	isVerbose := len(verbose) > 0 && verbose[0]
	logger := NewLogger(&outBuffer, &errBuffer, isVerbose)
	return logger, &outBuffer, &errBuffer
}

func usePlainProfile(t *testing.T) {
	t.Helper()
	SetProfile(ProfileNone)
	t.Cleanup(ResetProfile)
}

func TestLoggerPlainRecordGrammar(t *testing.T) {
	usePlainProfile(t)
	logger, out, errOut := newTestLogger(true)

	logger.Info("Deploying to %d hosts", 2)
	logger.Success("Deployment complete")
	logger.Warn("Digest verification disabled")
	logger.Debug("state file: %s", "/tmp/state")
	logger.Host("app-01", "Starting container")
	logger.HostSuccess("app-01", "Container started")
	logger.Step(2, 7, "Sync secrets")
	logger.Command("podman build --pull .")
	logger.Error("Readiness failed\nretry budget exhausted")
	logger.HostError("app-02", "Connection refused")

	const wantOut = "" +
		"  INFO   Deploying to 2 hosts\n" +
		"  OK     Deployment complete\n" +
		"  WARN   Digest verification disabled\n" +
		"  DEBUG  state file: /tmp/state\n" +
		"  HOST   app-01 / Starting container\n" +
		"  OK     app-01 / Container started\n" +
		"  STEP   2/7  Sync secrets\n" +
		"  CMD    podman build --pull .\n"
	if got := out.String(); got != wantOut {
		t.Fatalf("stdout:\n%q\nwant:\n%q", got, wantOut)
	}

	const wantErr = "" +
		"  ERROR  Readiness failed\n" +
		"         retry budget exhausted\n" +
		"  ERROR  app-02 / Connection refused\n"
	if got := errOut.String(); got != wantErr {
		t.Fatalf("stderr:\n%q\nwant:\n%q", got, wantErr)
	}
}

func TestHeaderUsesCompactRuleAndNoLeadingBlank(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()

	logger.Header("Deploy / image:v42")
	logger.Info("Ready")
	logger.Header("Health")

	const rule = "--------------------------------------------------------"
	want := "  # Deploy / image:v42\n  " + rule + "\n" +
		"  INFO   Ready\n\n" +
		"  # Health\n  " + rule + "\n"
	if got := out.String(); got != want {
		t.Fatalf("header output:\n%q\nwant:\n%q", got, want)
	}
}

func TestErrorDoesNotCreateLeadingBlankOnStdout(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()

	logger.Error("failed before output")
	logger.Header("Recovery")

	if got := out.String(); strings.HasPrefix(got, "\n") {
		t.Fatalf("first stdout header has a leading blank after stderr activity: %q", got)
	}
}

func TestEmptyCapturedOutputDoesNotCreateLeadingBlank(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()

	logger.Output("")
	logger.Header("First")

	if got := out.String(); strings.HasPrefix(got, "\n") {
		t.Fatalf("empty captured output marked stdout as started: %q", got)
	}
}

func TestOutputPreservesIndentationAndBlankLines(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()

	logger.Output("STEP 1\n\n  indented\n")

	const want = "" +
		"         | STEP 1\n" +
		"         | \n" +
		"         |   indented\n"
	if got := out.String(); got != want {
		t.Fatalf("captured output:\n%q\nwant:\n%q", got, want)
	}
}

func TestOutputErrorStaysOnStderr(t *testing.T) {
	usePlainProfile(t)
	logger, out, errOut := newTestLogger()
	logger.OutputError("diagnostic\n")

	if out.Len() != 0 {
		t.Fatalf("captured stderr leaked to stdout: %q", out.String())
	}
	if got, want := errOut.String(), "         | diagnostic\n"; got != want {
		t.Fatalf("captured stderr = %q, want %q", got, want)
	}
}

func TestTrafficBarPlainBoundariesAndClamp(t *testing.T) {
	tests := []struct {
		name       string
		percentage int
		wantBar    string
		wantSplit  string
	}{
		{name: "zero", percentage: 0, wantBar: "[--------------------------------]", wantSplit: "000/100"},
		{name: "quarter", percentage: 25, wantBar: "[########------------------------]", wantSplit: "025/075"},
		{name: "hundred", percentage: 100, wantBar: "[################################]", wantSplit: "100/000"},
		{name: "clamp low", percentage: -8, wantBar: "[--------------------------------]", wantSplit: "000/100"},
		{name: "clamp high", percentage: 108, wantBar: "[################################]", wantSplit: "100/000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usePlainProfile(t)
			logger, out, _ := newTestLogger()
			logger.TrafficBar(test.percentage, "canary (v2)", "stable (v1)")

			got := out.String()
			if !strings.Contains(got, test.wantBar+" "+test.wantSplit) {
				t.Fatalf("traffic bar %q does not contain %q", got, test.wantBar+" "+test.wantSplit)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("plain traffic bar contains ANSI: %q", got)
			}
		})
	}
}

func TestTrafficBarNarrowDropsGaugeButKeepsExactSplit(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.SetWidth(24)
	logger.TrafficBar(25, "canary", "stable")

	const want = "" +
		"  SPLIT  025/075\n" +
		"         25% canary /\n" +
		"         75% stable\n"
	if got := out.String(); got != want {
		t.Fatalf("narrow traffic split = %q, want %q", got, want)
	}
}

func TestHostPhasePlainUsesWrittenASCIIState(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.HostPhase("app-01", []Phase{
		{Name: "Pull", Complete: true},
		{Name: "Health", Complete: false},
	})

	const want = "  HOST   app-01 / [x] Pull  [ ] Health\n"
	if got := out.String(); got != want {
		t.Fatalf("phase output = %q, want %q", got, want)
	}
}

func TestStatusBadgePlainPairsStateWithText(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.StatusBadge("Status:", "rolling_back")

	const want = "  STATE  Status:          [ROLLING_BACK]\n"
	if got := out.String(); got != want {
		t.Fatalf("status output = %q, want %q", got, want)
	}
}

func TestTablePlainAlignmentHasNoTrailingWhitespace(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.Table(
		[]string{"Role", "Host", "Status"},
		[][]string{
			{"web", "app-01", "running"},
			{"jobs", "worker-01", "stopped"},
		},
	)

	const want = "" +
		"  Role  Host       Status\n" +
		"  ----  ---------  -------\n" +
		"  web   app-01     running\n" +
		"  jobs  worker-01  stopped\n"
	if got := out.String(); got != want {
		t.Fatalf("table output:\n%q\nwant:\n%q", got, want)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("data/rule line has trailing whitespace: %q", line)
		}
	}
}

func TestTableNarrowReflowsWithoutDroppingData(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.SetWidth(24)
	logger.Table(
		[]string{"Role", "Host", "Status"},
		[][]string{{"web", "production-app-01", "running"}},
	)

	const want = "" +
		"  REC    01/01\n" +
		"  Role\n" +
		"    web\n" +
		"  Host\n" +
		"    production-app-01\n" +
		"  Status\n" +
		"    running\n"
	if got := out.String(); got != want {
		t.Fatalf("narrow table:\n%q\nwant:\n%q", got, want)
	}
}

func TestEmptyNarrowTableUsesCompactRecord(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.SetWidth(12)
	logger.Table([]string{"Name", "Status"}, nil)

	const want = "" +
		"  REC    0\n" +
		"  Name\n" +
		"  Status\n"
	if got := out.String(); got != want {
		t.Fatalf("empty narrow table = %q, want %q", got, want)
	}
}

func TestTableMeasuresDisplayCellsInsteadOfBytes(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.Table([]string{"Name", "Zone"}, [][]string{{"café", "東京"}})

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table lines = %d, want 3: %q", len(lines), out.String())
	}
	if got, want := displayWidth(lines[0]), displayWidth(lines[1]); got != want {
		t.Fatalf("header width = %d, rule width = %d", got, want)
	}
}

func TestDisplayWidthMeasuresGraphemeClusters(t *testing.T) {
	tests := map[string]int{
		"e\u0301": 1,
		"❤️":      2,
		"🇪🇸":      2,
		"👨‍👩‍👧‍👦": 2,
	}
	for value, want := range tests {
		if got := displayWidth(value); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestTableSanitizesNestedANSIAndControlLayout(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.Table([]string{"State"}, [][]string{{"\x1b[31mfailed\x1b[0m\nretry\t2\x1b]0;title\a"}})

	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("table retained nested ANSI: %q", got)
	}
	if !strings.Contains(got, `failed\nretry    2`) {
		t.Fatalf("table did not expose escaped control layout: %q", got)
	}
	if !strings.Contains(got, `\x1b]0;title\x07`) {
		t.Fatalf("table did not visibly escape terminal controls: %q", got)
	}
}

func TestRichTerminalUsesThemeMappedLabelsAndPlainMessages(t *testing.T) {
	SetProfile(ProfileTrueColor)
	t.Cleanup(ResetProfile)
	logger, out, _ := newTestLogger()
	logger.Info("plain body")

	got := out.String()
	if !strings.Contains(got, "\x1b[1;34mINFO ") {
		t.Fatalf("missing theme-mapped semantic blue label: %q", got)
	}
	if strings.Contains(got, "38;2") || strings.Contains(got, "38;5") {
		t.Fatalf("terminal text bypassed the user's ANSI theme: %q", got)
	}
	if !strings.HasSuffix(got, "  plain body\n") {
		t.Fatalf("message should inherit terminal foreground: %q", got)
	}
}

func TestStatusBadgePadsLabelsByDisplayWidth(t *testing.T) {
	usePlainProfile(t)
	logger, out, _ := newTestLogger()
	logger.StatusBadge("État:", "running")

	const want = "  STATE  État:            [RUNNING]\n"
	if got := out.String(); got != want {
		t.Fatalf("Unicode status label = %q, want %q", got, want)
	}
}

func TestPlainSurfaceContainsNoANSIOrUnicodeStructure(t *testing.T) {
	usePlainProfile(t)
	logger, out, errOut := newTestLogger(true)
	logger.Header("Checks")
	logger.Info("Info")
	logger.Success("Success")
	logger.Warn("Warning")
	logger.Debug("Debug")
	logger.Command("command")
	logger.Output("output")
	logger.HostPhase("host", []Phase{{Name: "Pull", Complete: true}})
	logger.TrafficBar(50, "canary", "stable")
	logger.Error("Error")

	combined := out.String() + errOut.String()
	if strings.Contains(combined, "\x1b") {
		t.Fatalf("plain surface contains ANSI: %q", combined)
	}
	for _, symbol := range []string{SymHeader, SymFilled, SymEmpty, SymPending, SymRail} {
		if strings.Contains(combined, symbol) {
			t.Fatalf("plain surface contains Unicode structure %q: %q", symbol, combined)
		}
	}
}
