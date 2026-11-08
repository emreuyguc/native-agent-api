package permission_test

import (
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/permission"

	"github.com/google/go-cmp/cmp"
)

func TestStoreScopes(t *testing.T) {
	t.Parallel()
	s := permission.NewStore()
	s.Allow("w1", "s1", "command", permission.ScopeSession)
	s.Allow("w1", "s1", "file_change", permission.ScopeWorkspace)
	s.Allow("w1", "s1", "Read", permission.ScopeOnce)

	tests := []struct {
		workspace, session, tool string
		scope                    string
		ok                       bool
	}{
		{"w1", "s1", "command", permission.ScopeSession, true},
		{"w1", "s2", "command", "", false},
		{"w1", "s2", "file_change", permission.ScopeWorkspace, true},
		{"w2", "s1", "file_change", "", false},
		{"w1", "s1", "Read", "", false},
	}
	for _, tc := range tests {
		if scope, ok := s.Allowed(tc.workspace, tc.session, tc.tool); scope != tc.scope || ok != tc.ok {
			t.Errorf("Allowed(%s,%s,%s) = %q,%v", tc.workspace, tc.session, tc.tool, scope, ok)
		}
	}
	want := permission.Rules{Workspace: []string{"file_change"}, Sessions: map[string][]string{"s1": {"command"}}}
	if diff := cmp.Diff(want, s.Rules("w1")); diff != "" {
		t.Errorf("Rules mismatch (-want +got):\n%s", diff)
	}
	s.Clear("w1")
	if _, ok := s.Allowed("w1", "s2", "file_change"); ok {
		t.Error("rule survived Clear")
	}
	if !permission.ValidScope("session") || permission.ValidScope("global") {
		t.Error("ValidScope")
	}
}
