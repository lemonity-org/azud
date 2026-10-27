package secretsfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipModeChecksOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not available on Windows")
	}
}

func TestWriteFileCreatesPrivateFileAndAzudDir(t *testing.T) {
	skipModeChecksOnWindows(t)
	path := filepath.Join(t.TempDir(), ".azud", "secrets")

	result, err := WriteFile(path, []byte("A=1\n"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !result.Created || result.Tightened() {
		t.Fatalf("result = %+v, want a created file", result)
	}
	assertMode(t, filepath.Dir(path), 0o700)
	assertMode(t, path, 0o600)
	assertContent(t, path, "A=1\n")
}

func TestWriteFileTightensExistingInsecureFileAndAzudDir(t *testing.T) {
	skipModeChecksOnWindows(t)
	dir := filepath.Join(t.TempDir(), ".azud")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets")
	writeTestFile(t, path, "OLD=1\n", 0o644)

	result, err := WriteFile(path, []byte("NEW=2\n"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if result.Created || result.PreviousMode != 0o644 || !result.Tightened() {
		t.Fatalf("result = %+v, want a tightened 0644 file", result)
	}
	assertMode(t, path, 0o600)
	assertMode(t, dir, 0o700)
	assertContent(t, path, "NEW=2\n")
}

func TestWriteFileOnlyTightensAzudParent(t *testing.T) {
	skipModeChecksOnWindows(t)
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.env")

	if _, err := WriteFile(path, []byte("A=1\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertMode(t, dir, 0o755)
	assertMode(t, path, 0o600)
}

func TestWriteFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	writeTestFile(t, victim, "ORIGINAL\n", 0o600)
	path := filepath.Join(dir, "secrets")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := WriteFile(path, []byte("PWNED=1\n"))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("WriteFile error = %v, want a symbolic link refusal", err)
	}
	assertContent(t, victim, "ORIGINAL\n")
	assertNoTempFiles(t, dir)
}

func TestWriteFileRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created-by-link")
	path := filepath.Join(dir, "secrets")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := WriteFile(path, []byte("A=1\n")); err == nil {
		t.Fatal("expected a dangling symlink to be refused")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("link target was created: %v", err)
	}
}

func TestWriteFileDoesNotWriteThroughHardlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	writeTestFile(t, victim, "ORIGINAL\n", 0o600)
	path := filepath.Join(dir, "secrets")
	if err := os.Link(victim, path); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if _, err := WriteFile(path, []byte("NEW=1\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertContent(t, victim, "ORIGINAL\n")
	assertContent(t, path, "NEW=1\n")
}

func TestWriteFileRefusesDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := WriteFile(path, []byte("A=1\n"))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("WriteFile error = %v, want a non-regular file refusal", err)
	}
	assertNoTempFiles(t, dir)
}

func TestTightenRemovesGroupAndOtherBits(t *testing.T) {
	skipModeChecksOnWindows(t)
	path := filepath.Join(t.TempDir(), "secrets")
	writeTestFile(t, path, "A=1\n", 0o640)

	changed, err := Tighten(path)
	if err != nil || !changed {
		t.Fatalf("Tighten = %v, %v; want true, nil", changed, err)
	}
	assertMode(t, path, 0o600)

	changed, err = Tighten(path)
	if err != nil || changed {
		t.Fatalf("second Tighten = %v, %v; want false, nil", changed, err)
	}
}

func TestTightenRefusesSymlink(t *testing.T) {
	skipModeChecksOnWindows(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	writeTestFile(t, victim, "A=1\n", 0o644)
	path := filepath.Join(dir, "secrets")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Tighten(path); err == nil {
		t.Fatal("expected Tighten to refuse a symlink")
	}
	assertMode(t, victim, 0o644)
}

func TestAppendFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")

	if err := AppendFile(path, []byte("a\n"), 0o644); err != nil {
		t.Fatalf("AppendFile create: %v", err)
	}
	if err := AppendFile(path, []byte("b\n"), 0o644); err != nil {
		t.Fatalf("AppendFile append: %v", err)
	}
	assertContent(t, path, "a\nb\n")

	victim := filepath.Join(dir, "victim")
	writeTestFile(t, victim, "ORIGINAL\n", 0o600)
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := AppendFile(link, []byte("x\n"), 0o644); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("AppendFile through symlink error = %v, want refusal", err)
	}
	assertContent(t, victim, "ORIGINAL\n")
}

func TestFormat(t *testing.T) {
	tests := []struct {
		name    string
		secrets map[string]string
		want    string
		wantErr string
	}{
		{name: "sorted", secrets: map[string]string{"B": "2", "A": "1"}, want: "# h\nA=1\nB=2\n"},
		{name: "legacy key", secrets: map[string]string{"my-key": "v"}, want: "# h\nmy-key=v\n"},
		{name: "value with equals", secrets: map[string]string{"A": "x=y"}, want: "# h\nA=x=y\n"},
		{name: "newline in value", secrets: map[string]string{"A": "v\nINJECTED=1"}, wantErr: "newlines"},
		{name: "carriage return in value", secrets: map[string]string{"A": "v\rB=1"}, wantErr: "newlines"},
		{name: "nul in value", secrets: map[string]string{"A": "v\x00"}, wantErr: "NUL"},
		{name: "empty key", secrets: map[string]string{"": "v"}, wantErr: "empty"},
		{name: "equals in key", secrets: map[string]string{"A=B": "v"}, wantErr: "="},
		{name: "comment key", secrets: map[string]string{" #X": "v"}, wantErr: "#"},
		{name: "newline in key", secrets: map[string]string{"A\nB": "v"}, wantErr: "newlines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Format("# h\n", tt.secrets)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Format error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Format: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("Format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		key   string
		valid bool
	}{
		{"DATABASE_URL", true},
		{"_private", true},
		{"a1", true},
		{"", false},
		{"1A", false},
		{"A=B", false},
		{"#X", false},
		{"A B", false},
		{"my-key", false},
		{"A\nB", false},
	}
	for _, tt := range tests {
		err := ValidateKey(tt.key)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateKey(%q) = %v, valid=%v", tt.key, err, tt.valid)
		}
	}
}

func TestValidateValue(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\x00b", "trailing\n"} {
		if err := ValidateValue("K", value); err == nil {
			t.Errorf("ValidateValue(%q) succeeded, want error", value)
		}
	}
	for _, value := range []string{"", "plain", "with spaces", "x=y", "quote\"s", "unicodé"} {
		if err := ValidateValue("K", value); err != nil {
			t.Errorf("ValidateValue(%q) = %v", value, err)
		}
	}
}

func TestInsecureMode(t *testing.T) {
	skipModeChecksOnWindows(t)
	dir := t.TempDir()
	for perm, want := range map[os.FileMode]bool{0o600: false, 0o400: false, 0o640: true, 0o644: true, 0o602: true} {
		path := filepath.Join(dir, perm.String())
		writeTestFile(t, path, "", perm)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := InsecureMode(info); got != want {
			t.Errorf("InsecureMode(%04o) = %v, want %v", perm, got, want)
		}
	}
}

func writeTestFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".secrets-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}
