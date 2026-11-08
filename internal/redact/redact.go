// Package redact removes credentials and control characters from text that leaves the
// gateway (API error messages, logs).
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`),                             // Anthropic API keys and OAuth tokens
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),                           // OpenAI style API keys
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),             // HTTP bearer tokens
	regexp.MustCompile(`(?i)("(?:access|refresh|id)_?token"\s*:\s*")[^"]+`), // JSON token fields
}

// Placeholder replaces every secret.
const Placeholder = "[redacted]"

// Secrets replaces known credential formats with Placeholder.
func Secrets(text string) string {
	for _, pattern := range secretPatterns {
		if pattern.NumSubexp() > 0 {
			text = pattern.ReplaceAllString(text, "${1}"+Placeholder)
			continue
		}
		text = pattern.ReplaceAllString(text, Placeholder)
	}

	return text
}

// Message redacts secrets, strips control characters (except newline and tab), trims and
// truncates text to at most limit bytes on a UTF-8 boundary. A limit <= 0 means no limit.
func Message(text string, limit int) string {
	// Strip first: a control character inside a secret would otherwise hide it from the
	// patterns and disappear afterwards, revealing it (docs/security.md F-6).
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}

		return -1
	}, text)
	text = Secrets(text)
	if limit > 0 && len(text) > limit {
		text = Truncate(text, limit)
	}

	return strings.TrimSpace(text)
}

// Truncate cuts text to at most limit bytes without splitting a UTF-8 sequence.
func Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}

	return text
}
