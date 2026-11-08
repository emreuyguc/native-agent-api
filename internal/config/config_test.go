package config_test

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/config"

	"github.com/google/go-cmp/cmp"
)

func environ(pairs ...string) map[string]string {
	values := map[string]string{
		"HOME":                                 "/home/me",
		"USER":                                 "me",
		"PATH":                                 "/usr/local/bin:/usr/bin:/bin",
		"TMPDIR":                               "/tmp",
		"NATIVE_AGENT_AGENT":                   "claude",
		"NATIVE_AGENT_SANDBOX_MODE":            "cli",
		"NATIVE_AGENT_LISTEN":                  "127.0.0.1:8080",
		"NATIVE_AGENT_ALLOWED_IPS":             "",
		"NATIVE_AGENT_TOKEN":                   "",
		"NATIVE_AGENT_TOKEN_SHA256":            "",
		"NATIVE_AGENT_TOKEN_SHA256_FILE":       "",
		"NATIVE_AGENT_ALLOW_UNAUTHENTICATED":   "false",
		"NATIVE_AGENT_LOG_LEVEL":               "info",
		"NATIVE_AGENT_LOG_FORMAT":              "json",
		"NATIVE_AGENT_LOG_REQUESTS":            "true",
		"NATIVE_AGENT_LOG_BODIES":              "false",
		"NATIVE_AGENT_LOG_BODY_LIMIT":          "4096",
		"NATIVE_AGENT_DOCS":                    "true",
		"NATIVE_AGENT_DATA_DIR":                "/home/me/gateway",
		"NATIVE_AGENT_ALLOWED_ROOTS":           "",
		"NATIVE_AGENT_PROFILE_DIR":             "native",
		"NATIVE_AGENT_MAX_CONCURRENT":          "2",
		"NATIVE_AGENT_QUERY_TIMEOUT_SECONDS":   "480",
		"NATIVE_AGENT_COMMAND_TIMEOUT_SECONDS": "15",
		"NATIVE_AGENT_LOGIN_TIMEOUT_SECONDS":   "900",
		"NATIVE_AGENT_RUN_RETENTION_SECONDS":   "600",
		"NATIVE_AGENT_MAX_RETAINED_RUNS":       "200",
		"NATIVE_AGENT_MAX_RUN_EVENTS":          "10000",
		"NATIVE_AGENT_MAX_RUN_OBSERVATIONS":    "256",
		"NATIVE_AGENT_RUN_IDLE_SECONDS":        "300",
		"NATIVE_AGENT_MAX_BODY_BYTES":          "2000000",
		"NATIVE_AGENT_MAX_OUTPUT_BYTES":        "16000000",
		"NATIVE_AGENT_MAX_FILE_BYTES":          "536870912",
		"NATIVE_AGENT_MAX_LIST_ENTRIES":        "10000",
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		values[pairs[i]] = pairs[i+1]
	}

	return values
}

func TestLoadExplicitConfiguration(t *testing.T) {
	t.Parallel()

	got, err := config.Load(environ())
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		Agent:         "claude",
		SandboxMode:   "cli",
		Docs:          true,
		Network:       config.Network{Listen: "127.0.0.1:8080", AllowedIPs: []netip.Prefix{}},
		Log:           config.Log{Level: slog.LevelInfo, Format: "json", Requests: true, BodyLimit: 4096},
		HomeDir:       "/home/me",
		User:          "me",
		SearchPath:    "/usr/local/bin:/usr/bin:/bin",
		TempDir:       "/tmp",
		DataDir:       "/home/me/gateway",
		WorkDir:       "/home/me/gateway/work",
		WorkspaceRoot: "/home/me/gateway/workspaces",
		ProfileDir:    "native",
		Limits: config.Limits{
			MaxConcurrent:      2,
			QueryTimeout:       480 * time.Second,
			CommandTimeout:     15 * time.Second,
			LoginTimeout:       15 * time.Minute,
			RunRetention:       600 * time.Second,
			MaxRetainedRuns:    200,
			MaxRunEvents:       10_000,
			MaxRunObservations: 256,
			RunIdle:            300 * time.Second,
			MaxBodyBytes:       2_000_000,
			MaxOutputBytes:     16_000_000,
			MaxFileBytes:       512 << 20,
			MaxListEntries:     10_000,
		},
	}
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b netip.Prefix) bool { return a == b })); diff != "" {
		t.Fatalf("Load() mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadInteractiveRequiresOnlyInteractiveSettings(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"HOME": "/home/me", "USER": "me", "PATH": "/usr/bin", "TMPDIR": "/tmp",
		"NATIVE_AGENT_AGENT": "CLAUDE", "NATIVE_AGENT_DATA_DIR": "/data",
		"NATIVE_AGENT_PROFILE_DIR": "/srv/agent-profile",
	}
	got, err := config.LoadInteractive(env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "claude" || got.WorkDir != "/data/work" || got.ProfileDir != "/srv/agent-profile" {
		t.Fatalf("unexpected interactive config: %+v", got)
	}
	env["NATIVE_AGENT_PROFILE_DIR"] = ""
	if _, err := config.LoadInteractive(env); err == nil || !strings.Contains(err.Error(), "PROFILE_DIR must be native") {
		t.Fatalf("empty profile error = %v", err)
	}
	env["NATIVE_AGENT_PROFILE_DIR"] = "native"
	delete(env, "NATIVE_AGENT_DATA_DIR")
	if _, err := config.LoadInteractive(env); err == nil || !strings.Contains(err.Error(), "NATIVE_AGENT_DATA_DIR") {
		t.Fatalf("missing interactive setting error = %v", err)
	}
}

func TestLoadReadsFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	digestFile := filepath.Join(dir, "token.sha256")
	digest := config.HashToken(strings.Repeat("x", 32))
	writeFile(t, digestFile, hex.EncodeToString(digest[:])+"\n")

	cfg, err := config.Load(environ(
		"NATIVE_AGENT_TOKEN_SHA256_FILE", digestFile,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Enabled || cfg.Auth.TokenHash != digest || cfg.Auth.PlainToken {
		t.Fatalf("Auth=%+v", cfg.Auth)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	missingHome := environ()
	delete(missingHome, "HOME")
	missingAgent := environ()
	delete(missingAgent, "NATIVE_AGENT_AGENT")

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "missing home", env: missingHome, want: "HOME"},
		{name: "missing application setting", env: missingAgent, want: "NATIVE_AGENT_AGENT"},
		{name: "non integer", env: environ("NATIVE_AGENT_MAX_CONCURRENT", "two"), want: "MaxConcurrent"},
		{name: "out of range", env: environ("NATIVE_AGENT_MAX_CONCURRENT", "99"), want: "between 1 and 16"},
		{name: "unauthenticated override with token", env: environ("NATIVE_AGENT_LISTEN", "0.0.0.0:8080", "NATIVE_AGENT_TOKEN", strings.Repeat("x", 32), "NATIVE_AGENT_ALLOW_UNAUTHENTICATED", "true"), want: "contradicts a configured token"},
		{name: "bodies without request log", env: environ("NATIVE_AGENT_LOG_BODIES", "true", "NATIVE_AGENT_LOG_REQUESTS", "false"), want: "LOG_BODIES=true requires"},
		{name: "empty profile", env: environ("NATIVE_AGENT_PROFILE_DIR", ""), want: "PROFILE_DIR must be native or an absolute path"},
		{name: "relative profile", env: environ("NATIVE_AGENT_PROFILE_DIR", "profile"), want: "PROFILE_DIR must be native or an absolute path"},
		{name: "unknown sandbox mode", env: environ("NATIVE_AGENT_SANDBOX_MODE", "native"), want: "SANDBOX_MODE must be cli or external"},
		{name: "short token", env: environ("NATIVE_AGENT_TOKEN", "short"), want: "at least 32"},
		{name: "public listener without token", env: environ("NATIVE_AGENT_LISTEN", "0.0.0.0:8080"), want: "is required when listening"},
		{name: "hostname listener", env: environ("NATIVE_AGENT_LISTEN", "localhost:8080"), want: "must be ip:port"},
		{name: "two token forms", env: environ("NATIVE_AGENT_TOKEN", strings.Repeat("x", 32), "NATIVE_AGENT_TOKEN_SHA256", strings.Repeat("a", 64)), want: "only one of"},
		{name: "bad token digest", env: environ("NATIVE_AGENT_TOKEN_SHA256", "abc"), want: "64 hex"},
		{name: "missing secret file", env: environ("NATIVE_AGENT_TOKEN_SHA256_FILE", "/does/not/exist"), want: "TOKEN_SHA256_FILE"},
		{name: "bad allowlist", env: environ("NATIVE_AGENT_ALLOWED_IPS", "10.0.0.0/8,not-an-ip"), want: `"not-an-ip"`},
		{name: "bad log level", env: environ("NATIVE_AGENT_LOG_LEVEL", "loud"), want: "LogLevel"},
		{name: "bad bool", env: environ("NATIVE_AGENT_LOG_BODIES", "maybe"), want: "LogBodies"},
		{name: "relative data dir", env: environ("NATIVE_AGENT_DATA_DIR", "data"), want: "DATA_DIR must be an absolute path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadPublicListener(t *testing.T) {
	t.Parallel()

	token := strings.Repeat("x", config.MinTokenLength)
	digest := config.HashToken(token)
	tests := map[string][]string{
		"plain token":              {"NATIVE_AGENT_TOKEN", token},
		"token digest":             {"NATIVE_AGENT_TOKEN_SHA256", hex.EncodeToString(digest[:])},
		"explicit unauthenticated": {"NATIVE_AGENT_ALLOW_UNAUTHENTICATED", "true"},
	}
	t.Run("explicit unauthenticated on loopback", func(t *testing.T) {
		t.Parallel()
		if _, err := config.Load(environ("NATIVE_AGENT_ALLOW_UNAUTHENTICATED", "true")); err != nil {
			t.Fatal(err)
		}
	})
	for name, pair := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(environ(append(pair, "NATIVE_AGENT_LISTEN", "0.0.0.0:8080")...))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Auth.Enabled && cfg.Auth.TokenHash != digest {
				t.Fatal("token digest mismatch")
			}
		})
	}
}

func TestAllowlist(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(environ("NATIVE_AGENT_ALLOWED_IPS", "127.0.0.1, 10.1.0.0/16, ::1"))
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1":        true,
		"::ffff:127.0.0.1": true,
		"10.1.200.3":       true,
		"::1":              true,
		"10.2.0.1":         false,
		"192.168.1.10":     false,
	} {
		if got := cfg.Network.Allows(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", addr, got, want)
		}
	}
	if !(config.Network{}).Allows(netip.MustParseAddr("8.8.8.8")) {
		t.Error("empty allowlist must allow every address")
	}
}

func TestLoadReportsAllValidationErrors(t *testing.T) {
	t.Parallel()

	_, err := config.Load(environ("NATIVE_AGENT_MAX_CONCURRENT", "0", "NATIVE_AGENT_MAX_LIST_ENTRIES", "0"))
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) < 2 {
		t.Fatalf("expected multiple joined errors, got %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProxyVariablesPassOnlyWhenSet(t *testing.T) {
	t.Parallel()
	env := environ()
	env["HTTPS_PROXY"], env["no_proxy"], env["HTTP_PROXY"] = "http://egress:8888", "localhost", ""
	got, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"HTTPS_PROXY=http://egress:8888", "no_proxy=localhost"}; !slices.Equal(got.ProxyEnv, want) {
		t.Fatalf("ProxyEnv = %v, want %v", got.ProxyEnv, want)
	}
}
