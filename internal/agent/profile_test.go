package agent_test

import (
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

func TestResolveProfile(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		setting, wantDir string
		custom, fails    bool
	}{
		{setting: agent.ProfileNative, wantDir: "/home/me/.claude"},
		{setting: "/srv/profile/", wantDir: "/srv/profile", custom: true},
		{setting: "/home/me/.claude", fails: true},
		{setting: "/home/me/.claude/", fails: true},
		{setting: "relative", fails: true},
		{setting: "", fails: true},
	} {
		dir, custom, err := agent.ResolveProfile(test.setting, "/home/me", ".claude")
		if (err != nil) != test.fails || dir != test.wantDir || custom != test.custom {
			t.Errorf("ResolveProfile(%q) = %q, %v, %v", test.setting, dir, custom, err)
		}
	}
}
