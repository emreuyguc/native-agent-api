package agent_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

const testSessionID = "123e4567-e89b-42d3-a456-426614174000"

func TestNormalize(t *testing.T) {
	t.Parallel()

	workspaceID := strings.Repeat("a", 32)
	base := agent.Request{Prompt: "hi", Model: "sonnet", Effort: "medium"}
	workspace := base
	workspace.WorkspaceID = workspaceID
	workspace.SessionID = testSessionID
	workspace.SessionMode = agent.SessionNew
	workspace.Permissions = agent.PermissionsBypass
	tests := []struct {
		name    string
		req     agent.Request
		wantErr string
	}{
		{name: "model required", req: agent.Request{Prompt: "hi"}, wantErr: "model"},
		{name: "prompt required", req: agent.Request{Prompt: "  ", Model: "sonnet"}, wantErr: "prompt"},
		{name: "session without workspace", req: func() agent.Request { r := base; r.SessionID = testSessionID; return r }(), wantErr: "needs a workspace_id"},
		{name: "workspace without session", req: func() agent.Request { r := base; r.WorkspaceID = workspaceID; return r }(), wantErr: "session_id"},
		{name: "bad session mode", req: func() agent.Request { r := workspace; r.SessionMode = "bad"; return r }(), wantErr: "session_mode"},
		{name: "valid stateless", req: func() agent.Request { r := base; r.Model = "gpt-5.5/codex"; return r }()},
		{name: "valid workspace", req: workspace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.req.Normalize()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Normalize() = %v", err)
				}
				return
			}
			if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Normalize() = %v, want ErrInvalidRequest containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestNormalizeCanonicalizesFields(t *testing.T) {
	t.Parallel()

	req := agent.Request{
		Prompt: " hi ", Model: "sonnet", Effort: " medium ",
		WorkspaceID: strings.Repeat("a", 32), SessionID: strings.ToUpper(testSessionID), SessionMode: "new",
		Permissions: agent.PermissionsBypass,
	}
	if err := req.Normalize(); err != nil {
		t.Fatal(err)
	}
	if req.Prompt != "hi" || req.SessionID != testSessionID || req.Stateless() {
		t.Fatalf("unexpected normalized request %+v", req)
	}
}

func TestSplitProvider(t *testing.T) {
	t.Parallel()
	native, err := agent.SplitProvider(json.RawMessage(`{"name":"claude","max_turns":8}`), "provider", "claude")
	if err != nil || string(native) != `{"max_turns":8}` {
		t.Fatalf("native=%s err=%v", native, err)
	}
	if native, err = agent.SplitProvider(json.RawMessage(`{"name":"claude"}`), "provider", "claude"); err != nil || native != nil {
		t.Fatalf("only name: native=%s err=%v", native, err)
	}
	var mismatch *agent.ProviderMismatchError
	if _, err = agent.SplitProvider(json.RawMessage(`{"name":"codex","service_tier":"fast"}`), "provider", "claude"); !errors.As(err, &mismatch) {
		t.Fatalf("mismatch err = %v", err)
	}
	for _, raw := range []string{`{"max_turns":8}`, `[]`, `{"name":1}`} {
		if _, err = agent.SplitProvider(json.RawMessage(raw), "provider", "claude"); !errors.Is(err, agent.ErrInvalidRequest) {
			t.Fatalf("%s: err = %v", raw, err)
		}
	}
}

func TestProviderSectionJSON(t *testing.T) {
	t.Parallel()
	section := agent.NewProvider("claude", struct {
		Tool string `json:"tool"`
	}{"Bash"}, json.RawMessage(`{"x":1}`))
	encoded, err := json.Marshal(section)
	if err != nil || string(encoded) != `{"name":"claude","tool":"Bash","native":{"x":1}}` {
		t.Fatalf("encoded=%s err=%v", encoded, err)
	}
	var decoded agent.Provider
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Name != "claude" || string(decoded.Native) != `{"x":1}` {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if encoded, _ = json.Marshal(agent.NewProvider("codex", nil, nil)); string(encoded) != `{"name":"codex"}` {
		t.Fatalf("bare section = %s", encoded)
	}
}

// FuzzSplitProvider: never panics; accepted provider fields never carry "name" and are a JSON
// object.
func FuzzSplitProvider(f *testing.F) {
	for _, seed := range []string{`{"name":"claude","max_turns":1}`, `{"name":"claude"}`, `null`, `[]`, `{"name":"codex"}`, `{"name":"claude","name":"codex"}`, `{`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		native, err := agent.SplitProvider(json.RawMessage(raw), "provider", "claude")
		if err != nil || native == nil {
			return
		}
		var members map[string]json.RawMessage
		if json.Unmarshal(native, &members) != nil || members == nil {
			t.Fatalf("provider fields %s are not an object", native)
		}
		if _, ok := members["name"]; ok {
			t.Fatalf("provider fields %s keep name", native)
		}
	})
}

func TestUsageTotalPartial(t *testing.T) {
	t.Parallel()
	one := int64(1)
	total := agent.NewUsageTotal()
	total.Add(&agent.Tokens{Input: &one, CacheCreation: &one, Output: &one, CacheRead: &one})
	total.Add(&agent.Tokens{Input: &one, Output: &one})
	total.Add(nil)
	other := agent.NewUsageTotal()
	other.Add(&agent.Tokens{Output: &one, CacheRead: &one, CacheCreation: &one})
	total.Merge(other)
	if !slices.Equal(total.Partial, []string{"input", "cache_read", "cache_creation"}) || total.Turns != (agent.UsageTurns{Reported: 3, Unreported: 1}) ||
		*total.Tokens.Input != 2 || *total.Tokens.Output != 3 {
		t.Fatalf("total = %+v", total)
	}
}
