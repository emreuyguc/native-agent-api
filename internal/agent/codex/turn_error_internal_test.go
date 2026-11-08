package codex

import (
	"encoding/json"
	"testing"
)

func TestTurnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ native, want string }{
		{`{"message":"usage limit reached","codexErrorInfo":"usageLimitExceeded"}`, "usage limit reached"},
		{`{"codexErrorInfo":"other"}`, `{"codexErrorInfo":"other"}`},
		{`null`, "codex turn failed"},
		{``, "codex turn failed"},
	} {
		if got := turnError(json.RawMessage(tc.native), "failed"); got != tc.want {
			t.Errorf("turnError(%s) = %q, want %q", tc.native, got, tc.want)
		}
	}
}
