package localtest

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFakeAnswersFromHandle(t *testing.T) {
	fake := &Fake{Handle: func(cmd string, stdin io.Reader) (string, string, int) {
		data, _ := io.ReadAll(stdin)
		if cmd == "fail" {
			return "", "nope", 3
		}
		return cmd + ":" + string(data), "", 0
	}}

	result, err := fake.ExecuteWithStdin("host", "echo", strings.NewReader("in"))
	if err != nil || result.Stdout != "echo:in" || result.Host != "host" {
		t.Fatalf("ExecuteWithStdin = %+v, %v", result, err)
	}
	result, err = fake.Execute("host", "fail")
	if err != nil || result.Success() || result.Stderr != "nope" {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	var stdout, stderr bytes.Buffer
	if err := fake.ExecuteStream("host", "fail", &stdout, &stderr); err == nil || stderr.String() != "nope" {
		t.Fatalf("ExecuteStream = %v, stderr %q", err, stderr.String())
	}
	if got := strings.Join(fake.Commands(), ","); got != "echo,fail,fail" {
		t.Fatalf("Commands = %s", got)
	}
}

func TestFakeDefaultsToSuccess(t *testing.T) {
	fake := &Fake{UserName: "deploy"}
	results := fake.ExecuteParallel([]string{"a", "b"}, "anything")
	if len(results) != 2 || !results[0].Success() || results[1].Host != "b" || fake.User() != "deploy" {
		t.Fatalf("results = %+v", results)
	}
	ran := false
	if err := fake.WithRemoteLock("a", "/lock", time.Second, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("WithRemoteLock = %v, ran %v", err, ran)
	}
}

func TestFakeLockCanRefuse(t *testing.T) {
	refused := errors.New("held")
	fake := &Fake{Lock: func(string, time.Duration, func() error) error { return refused }}
	if err := fake.WithRemoteLock("a", "/lock", time.Second, func() error {
		t.Fatal("fn ran without the lock")
		return nil
	}); !errors.Is(err, refused) {
		t.Fatalf("WithRemoteLock = %v", err)
	}
}
