package cli_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
	"github.com/emreuyguc/native-agent-api/internal/cli"
	"github.com/emreuyguc/native-agent-api/internal/config"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), cli.Env{
		Args:    args,
		Environ: map[string]string{},
		Stdout:  &stdout,
		Stderr:  &stderr,
	})

	return code, stdout.String(), stderr.String()
}

func TestVersionAndHelp(t *testing.T) {
	t.Parallel()

	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, buildinfo.Name+" ") {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
}

func TestUnknownCommandExitsWithUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := run(t, "nope")
	if code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCommandIsRequired(t *testing.T) {
	t.Parallel()

	code, _, stderr := run(t)
	if code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestServeFailsFastOnInvalidConfig(t *testing.T) {
	t.Parallel()

	code, _, stderr := run(t, "serve")
	if code != 1 || !strings.Contains(stderr, `invalid configuration: env: required environment variable "NATIVE_AGENT_AGENT"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte("NATIVE_AGENT_AGENT=claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run(t, "serve", "--env-file", path)
	if code != 1 || !strings.Contains(stderr, `required environment variable "NATIVE_AGENT_LISTEN"`) {
		t.Fatalf("env file was not loaded: code=%d stderr=%q", code, stderr)
	}
}

func TestNewTokenPrintsMatchingDigest(t *testing.T) {
	t.Parallel()

	code, out, _ := run(t, "new-token")
	lines := strings.Split(out, "\n")
	if code != 0 || len(lines) < 5 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	token := strings.TrimSpace(lines[1])
	digest := strings.TrimPrefix(strings.TrimSpace(lines[4]), "NATIVE_AGENT_TOKEN_SHA256=")
	sum := config.HashToken(token)
	if len(token) < config.MinTokenLength || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("token=%q digest=%q", token, digest)
	}
}
