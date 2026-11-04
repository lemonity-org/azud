package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

func TestSSHClientPrintsThePushNoticeOnceOnStderr(t *testing.T) {
	previousLogger, previousConfig := output.DefaultLogger, cfg
	t.Cleanup(func() {
		output.DefaultLogger, cfg = previousLogger, previousConfig
		pushNotice = sync.Once{}
	})
	var stdout, stderr bytes.Buffer
	output.DefaultLogger = output.NewLogger(&stdout, &stderr, false)
	cfg = &config.Config{}
	pushNotice = sync.Once{}

	for range 3 {
		_ = createSSHClient().Close()
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q; a script may be capturing it", stdout.String())
	}
	if got := strings.Count(stderr.String(), "Azud 2.0 removes"); got != 1 {
		t.Errorf("notice printed %d times: %q", got, stderr.String())
	}
}
