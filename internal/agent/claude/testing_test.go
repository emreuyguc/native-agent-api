package claude_test

import (
	"os"
	"path/filepath"
	"testing"
)

// SuccessScript emulates `claude -p --input-format stream-json`: one turn per stdin line
// until stdin is closed.
const SuccessScript = `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' '{"type":"system","subtype":"init"}'
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"hello","total_cost_usd":0.01,"usage":{"input_tokens":3,"output_tokens":24,"cache_read_input_tokens":20,"cache_creation_input_tokens":4},"modelUsage":{"claude-sonnet":{"inputTokens":3,"outputTokens":24,"cacheReadInputTokens":20,"cacheCreationInputTokens":4,"costUSD":0.01,"contextWindow":200000,"maxOutputTokens":64000}}}'
done
`

// FakeCLI writes an executable shell script and returns its path.
func FakeCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}
