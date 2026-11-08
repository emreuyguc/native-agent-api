package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/config"
)

func TestMergeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, contents string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	path := write("valid.env", "# application settings\r\nNATIVE_AGENT_AGENT='claude'\r\nNATIVE_AGENT_LISTEN=127.0.0.1:8080 # local\r\nNATIVE_AGENT_TOKEN=\"file\\nvalue\"\r\nNATIVE_AGENT_ALLOWED_IPS=\r\nNATIVE_AGENT_DATA_DIR=/çalışma\r\n", 0o600)
	got, err := config.MergeFile(map[string]string{"NATIVE_AGENT_TOKEN": "process", "HOME": "/home/test"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if got["NATIVE_AGENT_AGENT"] != "claude" || got["NATIVE_AGENT_LISTEN"] != "127.0.0.1:8080" ||
		got["NATIVE_AGENT_TOKEN"] != "process" || got["NATIVE_AGENT_ALLOWED_IPS"] != "" || got["HOME"] != "/home/test" ||
		got["NATIVE_AGENT_DATA_DIR"] != "/çalışma" {
		t.Fatalf("unexpected merge: %#v", got)
	}
	if _, err := config.MergeFile(nil, filepath.Join(dir, "missing.env")); err == nil || !strings.Contains(err.Error(), "inspect env file") {
		t.Fatalf("missing file error = %v", err)
	}

	for _, tc := range []struct {
		name, contents, want string
		mode                 os.FileMode
	}{
		{name: "duplicate", contents: "NATIVE_AGENT_AGENT=claude\nNATIVE_AGENT_AGENT=codex\n", mode: 0o600, want: "duplicate"},
		{name: "foreign", contents: "PATH=/bin\n", mode: 0o600, want: "not an application variable"},
		{name: "unknown", contents: "NATIVE_AGENT_UNKNOWN=value\n", mode: 0o600, want: "unknown application variable"},
		{name: "syntax", contents: "NATIVE_AGENT_AGENT\n", mode: 0o600, want: "NAME=value"},
		{name: "permissions", contents: "NATIVE_AGENT_AGENT=claude\n", mode: 0o644, want: "permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.MergeFile(nil, write(tc.name+".env", tc.contents, tc.mode))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The shipped example must always be a complete, loadable application env file.
func TestEnvExampleLoads(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	system := map[string]string{"HOME": "/home/me", "USER": "me", "PATH": "/usr/bin:/bin", "TMPDIR": "/tmp"}
	merged, err := config.MergeFile(system, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(merged); err != nil {
		t.Fatalf("Load(.env.example) = %v", err)
	}
}
