package claude_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/claude"
)

const testSessionID = "123e4567-e89b-42d3-a456-426614174000"

func newClaude(t *testing.T, opts claude.Options) *claude.Agent {
	t.Helper()
	dir := t.TempDir()
	if opts.ProfileDir == "" {
		opts.ProfileDir = filepath.Join(dir, "profile")
	}
	if opts.HomeDir == "" {
		opts.HomeDir = dir
	}
	if opts.User == "" {
		opts.User = "test"
	}
	if opts.WorkDir == "" {
		opts.WorkDir = dir
	}
	if opts.TempDir == "" {
		opts.TempDir = dir
	}
	if opts.Path == "" {
		opts.Path = "/usr/bin:/bin"
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = 1_000_000
	}
	if opts.CommandTimeout == 0 {
		opts.CommandTimeout = 30 * time.Second
	}
	if opts.SandboxMode == "" {
		opts.SandboxMode = agent.SandboxExternal
	}
	opts.Logger = slog.New(slog.DiscardHandler)
	a, err := claude.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestValidateRequiresValuesAndAppliesLimits(t *testing.T) {
	t.Parallel()

	a := newClaude(t, claude.Options{})
	for _, bad := range []agent.Request{
		{Effort: "medium", Options: json.RawMessage(`{"max_turns":1001}`)},
		{Effort: "medium", Options: json.RawMessage(`{"max_output_tokens":100}`)},
	} {
		if err := a.Validate(&bad); !errors.Is(err, agent.ErrInvalidRequest) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidRequest", bad, err)
		}
	}
	// Every Claude-specific option is optional: omitted means native CLI behavior.
	for _, valid := range []agent.Request{
		{Effort: "medium"},
		{Effort: "medium", Options: json.RawMessage(`{"max_turns":8,"max_output_tokens":8192,"system_prompt_snapshot":"off","file_checkpointing":"off","debug":false}`)},
	} {
		if err := a.Validate(&valid); err != nil {
			t.Fatalf("Validate(%s) = %v", valid.Options, err)
		}
	}
}

func TestArguments(t *testing.T) {
	t.Parallel()

	base := agent.Request{Model: "sonnet", Effort: "medium", Options: json.RawMessage(`{"max_turns":4,"max_output_tokens":8192,"system_prompt_snapshot":"on","file_checkpointing":"off","debug":false}`)}
	tests := []struct {
		name        string
		mutate      func(*agent.Request)
		contains    []string
		notContains []string
	}{
		{
			name:        "stateless runs without tools or persistence",
			mutate:      func(*agent.Request) {},
			contains:    []string{"--restricted", "--tools  ", "--no-session-persistence", "dontAsk", "--strict-mcp-config", "--system-prompt-snapshot on"},
			notContains: []string{"--session-id", "--resume", "bypassPermissions"},
		},
		{
			name: "new workspace session is fully native",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionNew
				r.Permissions = agent.PermissionsBypass
			},
			contains:    []string{"--session-id " + testSessionID, "--tools default", "user,project,local", "bypassPermissions"},
			notContains: []string{"--restricted", "--strict-mcp-config", "--no-session-persistence"},
		},
		{
			name: "ask mode routes permission prompts to the gateway",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionNew
				r.Permissions = agent.PermissionsAsk
				r.Options = json.RawMessage(`{"permission_mode":"manual","system_prompt_snapshot":"on","file_checkpointing":"on","debug":false}`)
			},
			contains:    []string{"--permission-mode manual", "--permission-prompts host", "--permission-prompt-tool stdio", "--input-format stream-json"},
			notContains: []string{"bypassPermissions"},
		},
		{
			name: "fork",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionFork
				r.ForkFrom = "11111111-1111-4111-8111-111111111111"
				r.Permissions = agent.PermissionsBypass
			},
			contains: []string{"--resume 11111111-1111-4111-8111-111111111111 --fork-session --session-id " + testSessionID},
		},
		{
			name: "native options map to flags",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionNew
				r.Permissions = agent.PermissionsBypass
				r.Permissions = agent.PermissionsAsk
				r.Options = json.RawMessage(`{"permission_mode":"acceptEdits","system_prompt_snapshot":"off","file_checkpointing":"on","debug":true,"debug_filter":"hooks,mcp","tools":["Read","Edit"],"allowed_tools":["Bash(git *)"],
					"append_system_prompt":"be brief","max_budget_usd":0.5,"session_name":"demo","include_hook_events":true,
					"mcp_config":{"mcpServers":{}},"strict_mcp_config":true,"agents":{"r":{"description":"d","prompt":"p"}}}`)
			},
			contains: []string{
				"--permission-mode acceptEdits", "--tools Read,Edit", "--allowed-tools Bash(git *)", "--append-system-prompt be brief",
				"--max-budget-usd 0.5", "--name demo", "--include-hook-events", "--include-partial-messages",
				"--mcp-config /tmp/client.json", "--strict-mcp-config", "--debug=hooks,mcp", `--agents {"r":{"description":"d","prompt":"p"}}`,
			},
			notContains: []string{"bypassPermissions", "--tools default"},
		},
		{
			name: "extra directories and plugins",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionNew
				r.AddDirs = []string{"/srv/projects/lib", "/srv/projects/docs"}
				r.ResolveDir = func(dir string) (string, error) { return dir, nil }
				r.Options = json.RawMessage(`{"system_prompt_snapshot":"on","file_checkpointing":"on","debug":false,"plugin_dirs":["/srv/plugins/x"],"plugin_urls":["https://example.com/p.zip"]}`)
			},
			contains: []string{
				"--add-dir /srv/projects/lib --add-dir /srv/projects/docs",
				"--plugin-dir /srv/plugins/x", "--plugin-url https://example.com/p.zip",
			},
		},
		{
			name: "resume",
			mutate: func(r *agent.Request) {
				r.WorkspaceID, r.SessionID, r.SessionMode = strings.Repeat("a", 32), testSessionID, agent.SessionResume
				r.Permissions = agent.PermissionsBypass
			},
			contains:    []string{"--resume " + testSessionID},
			notContains: []string{"--session-id"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := base
			tc.mutate(&req)
			options, err := claude.ParseOptions(&req)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(claude.Arguments(req, options, agent.SandboxExternal, claude.Files{EmptyMCP: "/tmp/mcp.json", ClientMCP: "/tmp/client.json"}), " ")
			for _, want := range tc.contains {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in %s", want, joined)
				}
			}
			for _, unwanted := range tc.notContains {
				if strings.Contains(joined, unwanted) {
					t.Errorf("unexpected %q in %s", unwanted, joined)
				}
			}
		})
	}
}

func TestParseOptionsRejectsUnsafeOrInvalidValues(t *testing.T) {
	t.Parallel()
	workspace := agent.Request{WorkspaceID: strings.Repeat("a", 32), SessionID: testSessionID, SessionMode: agent.SessionNew, Permissions: agent.PermissionsBypass}
	tests := []struct {
		name    string
		req     agent.Request
		options string
		wantErr string
	}{
		{name: "unknown option", req: workspace, options: `{"shell":"rm"}`, wantErr: "unknown field"},
		{name: "stateless tools", req: agent.Request{}, options: `{"tools":["Bash"]}`, wantErr: "needs a workspace"},
		{name: "stateless mcp", req: agent.Request{}, options: `{"mcp_config":{}}`, wantErr: "needs a workspace"},
		{name: "mode without ask", req: workspace, options: `{"permission_mode":"plan"}`, wantErr: "needs permissions ask"},
		{name: "bad tool", req: workspace, options: `{"allowed_tools":["\u0001Bash"]}`, wantErr: "invalid tool rule"},
		{name: "flag in variadic tools", req: workspace, options: `{"allowed_tools":["Read","--settings={}"]}`, wantErr: "invalid tool rule"},
		{name: "flag in disallowed tools", req: workspace, options: `{"disallowed_tools":["-p"]}`, wantErr: "invalid tool rule"},
		{name: "flag in tools", req: workspace, options: `{"tools":["--mcp-config=x"]}`, wantErr: "invalid tool rule"},
		{name: "negative budget", req: workspace, options: `{"max_budget_usd":-1}`, wantErr: "must not be negative"},
		{name: "bad autocompact", req: workspace, options: `{"autocompact":"soon"}`, wantErr: "autocompact"},
		{name: "file escapes workspace", req: workspace, options: `{"files":["file_abc:../x"]}`, wantErr: "inside the workspace"},
		{name: "file without id", req: workspace, options: `{"files":["doc.txt"]}`, wantErr: "file_id"},
		{name: "stateless files", req: agent.Request{}, options: `{"files":["file_abc:a.txt"]}`, wantErr: "needs a workspace"},
		{name: "plugin dir without roots", req: workspace, options: `{"plugin_dirs":["/etc"]}`, wantErr: "ALLOWED_ROOTS"},
		{name: "plugin url not https", req: workspace, options: `{"plugin_urls":["http://example.com/p.zip"]}`, wantErr: "https"},
		{name: "plugin url with credentials", req: workspace, options: `{"plugin_urls":["https://u:p@example.com/p.zip"]}`, wantErr: "https"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := tc.req
			req.Options = json.RawMessage(tc.options)
			if _, err := claude.ParseOptions(&req); !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	stateless := agent.Request{Options: json.RawMessage(`{"append_system_prompt":"x","max_budget_usd":1}`)}
	if _, err := claude.ParseOptions(&stateless); err != nil {
		t.Fatalf("stateless safe options rejected: %v", err)
	}
	stateless.Options = json.RawMessage(`{"fallback_model":"sonnet, haiku","autocompact":"0.5M"}`)
	if _, err := claude.ParseOptions(&stateless); err != nil {
		t.Fatalf("pinned CLI option forms rejected: %v", err)
	}
}

func TestSettingsKeepGatewayPolicy(t *testing.T) {
	t.Parallel()
	req := agent.Request{
		Model: "sonnet", Effort: "low", WorkspaceID: strings.Repeat("a", 32), SessionID: testSessionID, SessionMode: agent.SessionNew,
		Options: json.RawMessage(`{"settings":{"env":{"A":"1"},"permissions":{"blockReadsOutsideWorkingDirectories":false,"allow":["Read"],"deny":["Bash(rm:*)"]},"sandbox":{"enabled":false,"failIfUnavailable":false,"excludedCommands":["git"]}}}`),
	}
	options, err := claude.ParseOptions(&req)
	if err != nil {
		t.Fatal(err)
	}
	for sandbox, policy := range map[agent.SandboxMode]string{
		agent.SandboxCLI:      `"sandbox":{"enabled":true,"excludedCommands":["git"],"failIfUnavailable":true}`,
		agent.SandboxExternal: `"sandbox":{"enabled":false,"excludedCommands":["git"],"failIfUnavailable":false}`,
	} {
		req.AddDirs = []string{"/srv/extra"}
		args := claude.Arguments(req, options, sandbox, claude.Files{})
		settings := args[slices.Index(args, "--settings")+1]
		for _, want := range []string{`"env":{"A":"1"}`, `"allow":["Read"]`, `"blockReadsOutsideWorkingDirectories":true`, `"fallbackModel":[]`, `"Bash(rm:*)"`, policy} {
			if !strings.Contains(settings, want) {
				t.Errorf("%s settings %s missing %s", sandbox, settings, want)
			}
		}
		// Under the CLI sandbox the file tools may not plant MCP servers, hooks or git hooks.
		denied := strings.Contains(settings, `"Edit(./.mcp.json)"`) && strings.Contains(settings, `"Edit(./.claude/**)"`) &&
			strings.Contains(settings, `"Edit(./.git/**)"`) && strings.Contains(settings, `"Edit(//srv/extra/.mcp.json)"`)
		if denied != (sandbox == agent.SandboxCLI) {
			t.Errorf("%s settings config deny rules = %v: %s", sandbox, denied, settings)
		}
	}
}

func TestEnvironmentDoesNotInheritGatewaySecrets(t *testing.T) {
	t.Setenv("NATIVE_AGENT_TOKEN", "must-not-reach-child")

	env := claude.RunEnvironment(claude.Options{HomeDir: "/home/x", ProfileDir: "/srv/profile"}, claude.RequestOptions{MaxOutputTokens: 512, FileCheckpointing: "on"})
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/srv/profile") {
		t.Fatalf("custom profile not exported: %v", env)
	}
	// The native profile must leave CLAUDE_CONFIG_DIR unset; setting it selects another
	// macOS Keychain entry and makes an existing login invisible.
	native := claude.Environment(claude.Options{HomeDir: "/home/x", ProfileDir: agent.ProfileNative})
	if slices.ContainsFunc(native, func(entry string) bool { return strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") }) {
		t.Fatalf("native profile exported CLAUDE_CONFIG_DIR: %v", native)
	}
	if claude.KeychainService("/home/x/.claude", false) != "Claude Code-credentials" ||
		!strings.HasPrefix(claude.KeychainService("/srv/profile", true), "Claude Code-credentials-") {
		t.Fatal("unexpected Keychain service names")
	}
	if slices.ContainsFunc(env, func(entry string) bool { return strings.Contains(entry, "must-not-reach-child") }) {
		t.Fatalf("gateway token leaked into child environment: %v", env)
	}
	if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "CLAUDE_CODE_SKIP_PROMPT_HISTORY=") }) {
		t.Fatal("CLAUDE_CODE_SKIP_PROMPT_HISTORY disables session transcripts and breaks resume")
	}
	if !slices.Contains(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS=512") {
		t.Fatalf("max output tokens missing: %v", env)
	}
	if !slices.Contains(env, "CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=1") {
		t.Fatalf("file checkpoint setting missing: %v", env)
	}
}

// converse starts a run, closes input after the first result and returns all events.
func converse(t *testing.T, a *claude.Agent, req agent.Request, onResult func(agent.Conversation)) ([]agent.Event, error) {
	t.Helper()
	var (
		events []agent.Event
		conv   agent.Conversation
		ready  = make(chan struct{})
	)
	conv, err := a.Start(t.Context(), req, func(event agent.Event) error {
		<-ready
		events = append(events, event)
		if event.Result != nil {
			if onResult != nil {
				onResult(conv)
			} else {
				_ = conv.CloseInput()
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	close(ready)
	err = conv.Wait()

	return events, err
}

func baseRequest() agent.Request {
	return agent.Request{
		Prompt: "hi", Model: "sonnet", Effort: "medium",
		Options: json.RawMessage(`{"system_prompt_snapshot":"on","file_checkpointing":"off","debug":false}`),
	}
}

func TestConversationStreamsEventsAndResults(t *testing.T) {
	t.Parallel()
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, SuccessScript), TempDir: t.TempDir(), WorkDir: t.TempDir()})

	turns := 0
	events, err := converse(t, a, baseRequest(), func(conv agent.Conversation) {
		turns++
		if turns == 1 {
			if err := conv.Send("again"); err != nil {
				t.Error(err)
			}
			return
		}
		_ = conv.CloseInput()
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	want := []string{"system", "assistant", "result", "system", "assistant", "result"}
	if !slices.Equal(types, want) || !events[0].TurnStarted || events[2].Result == nil || events[2].Result.Text != "hello" || events[2].Result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("types=%v first result=%+v", types, events[2].Result)
	}
	tokens, fields := events[2].Result.Tokens, events[2].Result.Provider.Fields.(claude.ResultFields)
	if tokens == nil || tokens.Input == nil || *tokens.Input != 3 || fields.Reason != "success" ||
		fields.Session.Models["claude-sonnet"].ContextWindow == nil {
		t.Fatalf("tokens = %+v, provider fields = %+v", tokens, fields)
	}
}

func TestTurnUsageAndContext(t *testing.T) {
	t.Parallel()
	// A subagent's call does not count toward the main agent's context; the turn's cost is the
	// sum of the native per-model usage since the previous result.
	script := `#!/bin/sh
read -r line
printf '%s\n' '{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"model":"claude-haiku","usage":{"input_tokens":9000}}}'
printf '%s\n' '{"type":"assistant","parent_tool_use_id":null,"message":{"model":"claude-sonnet","usage":{"input_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":10}}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.5,"modelUsage":{"claude-sonnet":{"contextWindow":200000},"claude-haiku":{"contextWindow":100000}},"model_usage_since_last_result":{"claude-sonnet":{"inputTokens":2,"costUSD":0.25},"claude-haiku":{"inputTokens":9000,"costUSD":0.125}}}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	events, err := converse(t, a, baseRequest(), func(conv agent.Conversation) { _ = conv.CloseInput() })
	if err != nil {
		t.Fatal(err)
	}
	result := events[len(events)-1].Result
	if result == nil || result.Context == nil || *result.Context != (agent.ContextUsage{Window: 200_000, Used: 112}) {
		t.Fatalf("result = %+v", result)
	}
	fields := result.Provider.Fields.(claude.ResultFields)
	if fields.Turn == nil || fields.Turn.CostUSD == nil || *fields.Turn.CostUSD != 0.375 || len(fields.Turn.Models) != 2 ||
		*fields.Turn.Models["claude-haiku"].Tokens.Input != 9000 || *fields.Session.CostUSD != 0.5 {
		t.Fatalf("provider fields = %+v", fields)
	}
}

func TestTaskNotificationResultIsAgentInitiated(t *testing.T) {
	t.Parallel()
	script := `#!/bin/sh
read -r prompt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":0,"origin":{"kind":"task-notification"}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok","origin":{"kind":"human"}}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	events, err := converse(t, a, baseRequest(), func(conv agent.Conversation) { _ = conv.CloseInput() })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !events[0].Result.AgentInitiated || events[1].Result.AgentInitiated || events[1].Result.Text != "ok" {
		t.Fatalf("results = %+v, %+v", events[0].Result, events[1].Result)
	}
}

func TestDebugOutputIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	script := `#!/bin/sh
echo 'debug token sk-ant-secret' >&2
read -r prompt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	req := baseRequest()
	req.Options = json.RawMessage(`{"system_prompt_snapshot":"on","file_checkpointing":"off","debug":true}`)
	events, err := converse(t, a, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Diagnostic == "" || strings.Contains(last.Diagnostic, "sk-ant-secret") || !strings.Contains(last.Diagnostic, "[redacted]") {
		t.Fatalf("diagnostic was not safely exposed: %+v", last)
	}
}

func TestConversationPermissionAndInterruptProtocol(t *testing.T) {
	t.Parallel()
	// Asks for permission, then echoes the next two stdin lines (our answer and interrupt).
	script := `#!/bin/sh
read -r prompt
printf '%s\n' '{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool","tool_name":"Write","description":"a.txt","input":{"file_path":"a.txt"}}}'
read -r answer
printf '{"type":"echo","line":%s}\n' "$answer"
read -r interrupt
printf '{"type":"echo","line":%s}\n' "$interrupt"
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"interrupted"}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	var (
		events []agent.Event
		conv   agent.Conversation
		ready  = make(chan struct{})
	)
	conv, err := a.Start(t.Context(), baseRequest(), func(event agent.Event) error {
		<-ready
		events = append(events, event)
		switch {
		case event.Permission != nil:
			if err := conv.Answer(agent.PermissionAnswer{ID: "unknown"}); !errors.Is(err, agent.ErrUnknownPermission) {
				t.Errorf("unknown answer error = %v", err)
			}
			if err := conv.Answer(agent.PermissionAnswer{ID: event.Permission.ID, Allow: true}); err != nil {
				t.Error(err)
			}
		case event.Type == "echo" && len(events) == 2:
			if err := conv.Interrupt(); err != nil {
				t.Error(err)
			}
		case event.Result != nil:
			_ = conv.CloseInput()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	if err := conv.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Permission == nil || events[0].Permission.Kind != agent.PermissionFileChange ||
		events[0].Permission.Summary != "a.txt" || events[0].Permission.Provider.Fields.(claude.PermissionFields).Tool != "Write" {
		t.Fatalf("events = %+v", events)
	}
	answer, interrupt := string(events[1].Raw), string(events[2].Raw)
	if !strings.Contains(answer, `"type":"control_response"`) || !strings.Contains(answer, `"request_id":"p1"`) ||
		!strings.Contains(answer, `"subtype":"success"`) || !strings.Contains(answer, `"behavior":"allow","updatedInput":{"file_path":"a.txt"}`) {
		t.Errorf("permission answer = %s", answer)
	}
	if !strings.Contains(interrupt, `"type":"control_request"`) || !strings.Contains(interrupt, `"subtype":"interrupt"`) {
		t.Errorf("interrupt = %s", interrupt)
	}
	if result := events[3].Result; result == nil || result.Outcome != agent.OutcomeInterrupted ||
		result.Provider.Fields.(claude.ResultFields).Reason != "error_during_execution" {
		t.Errorf("result = %+v", result)
	}
}

func TestConversationControlInBothDirections(t *testing.T) {
	t.Parallel()
	// Answers our control request, sends its own (hook callback), echoes our answer.
	script := `#!/bin/sh
read -r prompt
read -r control
id=$(printf '%s' "$control" | sed -E 's/.*"request_id":"([^"]+)".*/\1/')
printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"ok":true}}}\n' "$id"
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta"}}'
printf '%s\n' '{"type":"control_request","request_id":"h1","request":{"subtype":"hook_callback","callback_id":"cb"}}'
read -r answer
printf '{"type":"echo","line":%s}\n' "$answer"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	var (
		events []agent.Event
		conv   agent.Conversation
		ready  = make(chan struct{})
	)
	conv, err := a.Start(t.Context(), baseRequest(), func(event agent.Event) error {
		<-ready
		events = append(events, event)
		switch {
		case event.Control != nil:
			if err := conv.Respond(event.Control.ID, json.RawMessage(`{"continue":true}`), ""); err != nil {
				t.Error(err)
			}
		case event.Result != nil:
			_ = conv.CloseInput()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	response, err := conv.Control(t.Context(), json.RawMessage(`{"subtype":"get_context_usage"}`))
	if err != nil || string(response) != `{"ok":true}` {
		t.Fatalf("Control = %s, %v", response, err)
	}
	if err := conv.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || !events[0].Partial || events[1].Control == nil || events[1].Control.Provider.Fields.(claude.ControlFields).Subtype != "hook_callback" {
		t.Fatalf("events = %+v", events)
	}
	if echo := string(events[2].Raw); !strings.Contains(echo, `"request_id":"h1"`) || !strings.Contains(echo, `"response":{"continue":true}`) {
		t.Fatalf("hook answer = %s", echo)
	}
	if _, err := conv.Control(t.Context(), json.RawMessage(`{"subtype":"x"}`)); err == nil {
		t.Fatal("Control after exit succeeded")
	}
}

func TestConversationReportsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "invalid json", script: "#!/bin/sh\necho not-json\n", wantErr: claude.ErrInvalidStream.Error()},
		{name: "missing result", script: "#!/bin/sh\nread -r line\necho '{\"type\":\"assistant\"}'\n", wantErr: claude.ErrNoResult.Error()},
		{name: "non zero exit", script: "#!/bin/sh\necho 'failure sk-ant-secret' >&2\nexit 3\n", wantErr: "failure [redacted]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newClaude(t, claude.Options{Binary: FakeCLI(t, tc.script), TempDir: t.TempDir()})
			_, err := converse(t, a, baseRequest(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestErrorResultIsRedacted(t *testing.T) {
	t.Parallel()
	script := "#!/bin/sh\nread -r line\necho '{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"result\":\"boom sk-ant-secret\"}'\ncat >/dev/null\n"
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	events, err := converse(t, a, baseRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := events[0].Result; result == nil || result.Outcome != agent.OutcomeFailed || result.Text != "" || result.Error != "boom [redacted]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuthStatus(t *testing.T) {
	t.Parallel()

	ok := newClaude(t, claude.Options{Binary: FakeCLI(t, "#!/bin/sh\necho '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"email\":\"dev@example.test\",\"subscriptionType\":\"pro\"}'\n")})
	if status, err := ok.AuthStatus(t.Context()); err != nil || !status.LoggedIn || status.Provider.Fields.(claude.AuthFields).Method != "claude.ai" ||
		status.Account == nil || status.Account.Email != "dev@example.test" || status.Account.Plan != "pro" {
		t.Fatalf("AuthStatus() = %+v, %v", status, err)
	}
	// Claude prints its status and exits 1 when logged out: that is a result, not an error.
	loggedOut := newClaude(t, claude.Options{Binary: FakeCLI(t, "#!/bin/sh\necho '{\"loggedIn\":false,\"authMethod\":\"none\"}'\nexit 1\n")})
	if status, err := loggedOut.AuthStatus(t.Context()); err != nil || status.LoggedIn || status.Account != nil {
		t.Fatalf("AuthStatus(logged out) = %+v, %v", status, err)
	}
	broken := newClaude(t, claude.Options{Binary: FakeCLI(t, "#!/bin/sh\necho 'boom' >&2\nexit 1\n")})
	if _, err := broken.AuthStatus(t.Context()); !errors.Is(err, agent.ErrUpstream) {
		t.Fatalf("AuthStatus(broken) error = %v, want ErrUpstream", err)
	}
}

func TestModelsUseNativeInitialize(t *testing.T) {
	t.Parallel()

	script := `#!/bin/sh
case " $* " in *" --tools  "*"--setting-sources user"*disableAllHooks*) ;; *) echo "unsafe discovery flags: $*" >&2; exit 3 ;; esac
IFS= read -r line
case "$line" in *'"subtype":"initialize"'*) ;; *) exit 4 ;; esac
printf '%s\n' '{"type":"system","subtype":"hook_started"}'
printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"native-agent-api-models","response":{"models":[{"value":"opus","displayName":"Opus","supportsEffort":true}]}}}'
sleep 30
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script)})
	models, err := a.Models(t.Context())
	if err != nil || len(models) != 1 || models[0].ID != "opus" || models[0].Name != "Opus" || !strings.Contains(string(models[0].Provider.Native), "supportsEffort") {
		t.Fatalf("Models() = %+v, %v", models, err)
	}
}

func TestLoginPassesPastedCodeToNativeCLI(t *testing.T) {
	t.Parallel()

	script := `#!/bin/sh
[ "$*" = "auth login --claudeai --email me@example.com" ] || { echo "bad args: $*" >&2; exit 5; }
echo "Opening browser to sign in…"
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&state=abc"
printf 'Paste code here if prompted > '
IFS= read -r code
[ "$code" = "good#state" ] && exit 0
echo "Login failed: That code wasn't accepted."
exit 1
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script)})
	request := agent.LoginRequest{Method: claude.LoginClaudeAI, Options: json.RawMessage(`{"email":"me@example.com"}`)}
	for code, wantErr := range map[string]string{"good#state": "", "stale#state": "wasn't accepted"} {
		login, err := a.StartLogin(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if challenge := login.Challenge(); challenge.URL != "https://claude.com/cai/oauth/authorize?code=true&state=abc" || challenge.Flow != agent.FlowPasteCode {
			t.Fatalf("challenge = %+v", challenge)
		}
		if err := login.SubmitCode(code); err != nil {
			t.Fatal(err)
		}
		if err := login.SubmitCode(code); !errors.Is(err, agent.ErrInvalidRequest) {
			t.Fatalf("second code error = %v", err)
		}
		err = login.Wait()
		if (wantErr == "" && err != nil) || (wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr))) {
			t.Fatalf("code %q: Wait() = %v, want %q", code, err, wantErr)
		}
	}
}

func TestReadOAuthAccessTokenReturnsOnlySubscriptionToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-example","refreshToken":"secret"},"other":"secret"}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := claude.ReadOAuthAccessToken(dir)
	if err != nil || token != "sk-ant-oat01-example" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if _, err := claude.ReadOAuthAccessToken(t.TempDir()); !errors.Is(err, agent.ErrLoginRequired) {
		t.Fatalf("missing credentials error = %v", err)
	}
}

// Claude's task lifecycle events become observations; a limit result is limit_reached.
func TestTaskObservationsAndLimitOutcome(t *testing.T) {
	t.Parallel()
	script := `#!/bin/sh
read -r line
printf '%s\n' '{"type":"system","subtype":"task_started","task_id":"t1","description":"Explore repo","task_type":"local_agent","subagent_type":"Explore"}'
printf '%s\n' '{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"description":"still going"}}'
printf '%s\n' '{"type":"system","subtype":"task_notification","task_id":"t1","status":"completed"}'
printf '%s\n' '{"type":"result","subtype":"error_max_turns","is_error":true,"result":""}'
cat >/dev/null
`
	a := newClaude(t, claude.Options{Binary: FakeCLI(t, script), TempDir: t.TempDir()})
	events, err := converse(t, a, baseRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var observations []agent.Observation
	for _, event := range events {
		if event.Observation != nil {
			observations = append(observations, *event.Observation)
		}
	}
	if len(observations) != 2 || observations[0].Kind != agent.ObservationSubagent || observations[0].Label != "Explore repo" ||
		observations[0].State != agent.ObservationActive || observations[1].ID != "t1" || observations[1].State != agent.ObservationCompleted {
		t.Fatalf("observations = %+v", observations)
	}
	if result := events[len(events)-1].Result; result == nil || result.Outcome != agent.OutcomeLimitReached {
		t.Fatalf("result = %+v", result)
	}
}
