package redact_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// FuzzMessage: the output is valid UTF-8 when the input is, fits the limit, holds no control
// characters, and redacting it again changes nothing (no secret survives or appears late).
func FuzzMessage(f *testing.F) {
	for _, seed := range []string{
		"token sk-ant-abc123 leaked", "Authorization: Bearer abcdefghijkl", `{"access_token":"x"}`,
		"sk-\x01ant-oat01-secret", "Bear\x00er abcdefghijklmnop", "sk-proj-" + strings.Repeat("a", 30), "ünïcödé\x7f",
	} {
		f.Add(seed, 64)
	}
	f.Fuzz(func(t *testing.T, text string, limit int) {
		out := redact.Message(text, limit)
		if limit > 0 && len(out) > limit {
			t.Fatalf("len %d > limit %d", len(out), limit)
		}
		if utf8.ValidString(text) && !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 output %q", out)
		}
		for _, r := range out {
			if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
				t.Fatalf("control character %q in %q", r, out)
			}
		}
		// Checked without truncation: a cut placeholder ("[redacte") is not a secret.
		if full := redact.Message(text, 0); redact.Secrets(full) != full {
			t.Fatalf("secret survived redaction: %q -> %q", text, full)
		}
	})
}
