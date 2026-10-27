package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/output"
)

func TestStopProxyHostsAggregatesFailures(t *testing.T) {
	tests := []struct {
		name        string
		failures    map[string]error
		wantErr     []string
		wantSuccess bool
	}{
		{
			name:        "all hosts stopped",
			wantSuccess: true,
		},
		{
			name:     "one host failed",
			failures: map[string]error{"10.0.0.2": errors.New("simulated stop failure")},
			wantErr:  []string{"proxy stop failed on 1 host(s)", "10.0.0.2: simulated stop failure"},
		},
		{
			name: "every host failed",
			failures: map[string]error{
				"10.0.0.1": errors.New("first failure"),
				"10.0.0.2": errors.New("second failure"),
			},
			wantErr: []string{"proxy stop failed on 2 host(s)", "10.0.0.1: first failure", "10.0.0.2: second failure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := output.NewLogger(&buf, &buf, false)
			hosts := []string{"10.0.0.1", "10.0.0.2"}
			var attempted []string
			err := stopProxyHosts(hosts, func(host string) error {
				attempted = append(attempted, host)
				return tt.failures[host]
			}, log)

			if !reflect.DeepEqual(attempted, hosts) {
				t.Fatalf("attempted hosts = %v, want every host %v", attempted, hosts)
			}
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("stopProxyHosts returned %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("stopProxyHosts succeeded despite failures")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q missing %q", err, want)
					}
				}
			}
			if got := strings.Contains(buf.String(), "Proxy stopped"); got != tt.wantSuccess {
				t.Fatalf("success line logged = %v, want %v:\n%s", got, tt.wantSuccess, buf.String())
			}
		})
	}
}
