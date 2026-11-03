//go:build !windows

package local

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// When set, the test binary calls Interact instead of running tests.
const interactHelperEnv = "AZUD_TEST_INTERACT"

func TestMain(m *testing.M) {
	if os.Getenv(interactHelperEnv) != "" {
		err := Interact([]string{"sh", "-c", "echo replaced; exit 7"})
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestInteractRefusesAStdinThatIsNotATerminal(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), interactHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "connect with ssh -t, or drop -t") {
		t.Fatalf("err = %v, output = %q", err, out)
	}
}

func TestInteractReplacesTheProcess(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1), which provides the terminal, is not installed")
	}
	self := strconv.Quote(os.Args[0])
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" {
		cmd = exec.Command("script", "-qec", self, "/dev/null")
	} else {
		cmd = exec.Command("script", "-q", "/dev/null", os.Args[0])
	}
	cmd.Env = append(os.Environ(), interactHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(string(out), "replaced") {
		t.Fatalf("err = %v, output = %q; want the replacement's output and exit code", err, out)
	}
}

func TestInteractWithNothingToRun(t *testing.T) {
	if err := Interact(nil); err == nil {
		t.Fatal("Interact(nil) succeeded")
	}
}
