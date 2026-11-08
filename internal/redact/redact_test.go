package redact_test

import (
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/redact"
)

func TestSecrets(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"key sk-ant-oat01-abc_DEF-123 end":                  "key [redacted] end",
		"openai sk-proj-ABCDEFGHIJKLMNOPQRSTUV":             "openai [redacted]",
		"Authorization: Bearer abcdefgh12345678":            "Authorization: Bearer [redacted]",
		`{"access_token":"xyz","refreshToken":"abc","n":1}`: `{"access_token":"[redacted]","refreshToken":"[redacted]","n":1}`,
		"nothing secret here, skip-this-word":               "nothing secret here, skip-this-word",
	}
	for input, want := range tests {
		if got := redact.Secrets(input); got != want {
			t.Errorf("Secrets(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMessage(t *testing.T) {
	t.Parallel()

	if got := redact.Message("  bad\x1b[31m\x00 text  ", 0); got != "bad[31m text" {
		t.Errorf("control characters: %q", got)
	}
	if got := redact.Message(strings.Repeat("ç", 10), 5); got != "çç" {
		t.Errorf("utf-8 truncation: %q", got)
	}
}
