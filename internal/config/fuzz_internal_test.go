package config

import (
	"strings"
	"testing"
)

// FuzzParseEnvLine: parsing never panics, and an accepted line names a known application
// variable with a value free of NUL.
func FuzzParseEnvLine(f *testing.F) {
	for _, seed := range []string{
		"NATIVE_AGENT_AGENT=claude", `NATIVE_AGENT_LISTEN="127.0.0.1:8080" # comment`, "NATIVE_AGENT_TOKEN='a b'",
		"# comment", "", "PATH=/bin", "NATIVE_AGENT_AGENT=\"unterminated", "NATIVE_AGENT_AGENT=a\x00b", `NATIVE_AGENT_AGENT="a\"b"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		name, value, present, err := parseEnvLine(line)
		if err != nil || !present {
			return
		}
		if _, known := applicationEnvNames[name]; !known || !strings.HasPrefix(name, EnvPrefix) || strings.IndexByte(value, 0) >= 0 {
			t.Fatalf("parseEnvLine(%q) = %q, %q", line, name, value)
		}
	})
}
