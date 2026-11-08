package ids_test

import (
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/ids"
)

func TestGeneratedIDsAreValidAndUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{})
	for range 100 {
		workspace, session := ids.NewWorkspaceID(), ids.NewSessionID()
		if !ids.ValidWorkspaceID(workspace) {
			t.Fatalf("invalid workspace id %q", workspace)
		}
		if !ids.ValidSessionID(session) {
			t.Fatalf("invalid session id %q", session)
		}
		if run := ids.NewRunID(); !ids.ValidRunID(run) {
			t.Fatalf("invalid run id %q", run)
		}
		if _, dup := seen[workspace]; dup {
			t.Fatalf("duplicate workspace id %q", workspace)
		}
		seen[workspace] = struct{}{}
	}
}

func TestValidatorsRejectMalformedInput(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"", "../etc", "ABCDEF0123456789ABCDEF0123456789", "123E4567-E89B-42D3-A456-426614174000", "{123e4567-e89b-42d3-a456-426614174000}"} {
		if ids.ValidWorkspaceID(id) || ids.ValidSessionID(id) {
			t.Errorf("expected %q to be rejected", id)
		}
	}
}
