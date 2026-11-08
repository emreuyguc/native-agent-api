package codex_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/codex"
)

func TestMaintenanceSurfaceUsesExplicitEnvironment(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codex-test")
	script := `#!/bin/sh
if [ -n "$NATIVE_AGENT_TEST_SECRET" ]; then echo inherited secret >&2; exit 9; fi
case "$1" in
  --version) echo "codex-cli 0.167.0" ;;
  doctor) echo '{"status":"ok"}' ;;
  login) echo "Logged in using ChatGPT" ;;
  *) echo "unexpected command" >&2; exit 8 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATIVE_AGENT_TEST_SECRET", "must-not-leak")
	adapter, err := codex.New(codex.Options{
		Binary: binary, ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := adapter.Version(context.Background())
	if err != nil || strings.TrimSpace(version) != "codex-cli 0.167.0" {
		t.Fatalf("version=%q err=%v", version, err)
	}
	diagnostics, err := adapter.Diagnostics(context.Background())
	if err != nil || diagnostics.Format != "json" || !json.Valid([]byte(diagnostics.Output)) {
		t.Fatalf("diagnostics=%q err=%v", diagnostics, err)
	}
}

func TestPinnedSurfaceMatchesDescriptor(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/0.167.0-surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var surface struct {
		Version        string   `json:"version"`
		ClientRequests []string `json:"client_requests"`
		ServerRequests []string `json:"server_requests"`
	}
	if err := json.Unmarshal(data, &surface); err != nil {
		t.Fatal(err)
	}
	if surface.Version != codex.Descriptor.TestedVersion || len(surface.ClientRequests) == 0 || len(surface.ServerRequests) == 0 {
		t.Fatalf("surface=%+v descriptor=%+v", surface, codex.Descriptor)
	}
}

func TestValidateRejectsUnenforceableFields(t *testing.T) {
	t.Parallel()
	adapter := &codex.Agent{}
	requests := []agent.Request{
		{Prompt: "hi", Model: "gpt-test", Effort: "high", Options: json.RawMessage(`{"config":{"model_verbosity":"low"}}`)},
		{Prompt: "hi", Model: "gpt-test", Effort: "high", Options: json.RawMessage(`{"skill_roots":["/tmp"]}`)},
		{Prompt: "hi", Model: "gpt-test", Effort: "high", Options: json.RawMessage(`{"approvals_reviewer":"user"}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"max_turns":1}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"unknown":true}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"config":{"sandbox_mode":"danger-full-access"}}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"config":{"sandbox_workspace_write.network_access":true}}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"config":{"profile":"unsafe"}}`)},
		{WorkspaceID: "workspace", Permissions: agent.PermissionsBypass, Options: json.RawMessage(`{"approvals_reviewer":"auto_review"}`)},
		{WorkspaceID: "workspace", Options: json.RawMessage(`{"skill_roots":["/etc"]}`)},
	}
	for _, request := range requests {
		if err := adapter.Validate(&request); err == nil {
			t.Fatalf("Validate(%+v) succeeded", request)
		}
	}
}

func TestAppServerConversationLifecycle(t *testing.T) {
	root := t.TempDir()
	binary := helperBinary(t, root)
	adapter, err := codex.New(codex.Options{
		Binary: binary, ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	extraRoot := filepath.Join(root, "shared")
	if err := os.Mkdir(extraRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	events := make(chan agent.Event, 16)
	conversation, err := adapter.Start(t.Context(), agent.Request{
		Prompt: "hello", Model: "gpt-test", Effort: "high", OutputSchema: map[string]any{"type": "string"},
		WorkspaceID: strings.Repeat("a", 32), SessionID: "123e4567-e89b-42d3-a456-426614174000",
		SessionMode: agent.SessionNew, Permissions: agent.PermissionsAsk,
		WorkspaceDir: root, AddDirs: []string{extraRoot},
		ResolveDir: func(dir string) (string, error) { return dir, nil },
		Options: json.RawMessage(`{"config":{"mcp_servers":{"docs":{"command":"docs-mcp"}}},"developer_instructions":"be brief",` +
			`"approvals_reviewer":"user","reasoning_summary":"concise","disabled_plugin_ids":[],"skill_roots":["` + extraRoot + `"]}`),
	}, func(event agent.Event) error {
		events <- event

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := conversation.(agent.NativeSession)
	if !ok || native.NativeSessionID() != "native-thread" {
		t.Fatalf("native session = %T %q", conversation, native.NativeSessionID())
	}
	control, err := conversation.Control(t.Context(), json.RawMessage(`{"method":"skills/list","params":{}}`))
	if err != nil || !strings.Contains(string(control), `"skill-1"`) {
		t.Fatalf("control=%s err=%v", control, err)
	}

	var result *agent.Result
	var sawPartial, sawObservation, sawTurnStart, sawControl bool
	deadline := time.After(3 * time.Second)
	for result == nil {
		select {
		case event := <-events:
			sawPartial = sawPartial || event.Partial
			if event.Type == "item/commandExecution/outputDelta" && !event.Partial {
				t.Fatal("command output deltas must be live-only, or long outputs fill the event limit")
			}
			sawObservation = sawObservation || event.Observation != nil
			sawTurnStart = sawTurnStart || event.TurnStarted
			if event.Permission != nil {
				if event.Permission.Kind != agent.PermissionCommand || !strings.Contains(event.Permission.Summary, "pwd") ||
					event.Permission.Provider.Fields.(codex.RequestFields).Method != "item/commandExecution/requestApproval" {
					t.Fatalf("permission = %+v", event.Permission)
				}
				if err := conversation.Answer(agent.PermissionAnswer{ID: event.Permission.ID, Allow: true}); err != nil {
					t.Fatal(err)
				}
			}
			if event.Control != nil {
				sawControl = true
				if event.Control.Provider.Fields.(codex.RequestFields).Method != "item/tool/call" {
					t.Fatalf("control request = %+v", event.Control)
				}
				if err := conversation.Respond(event.Control.ID, json.RawMessage(`{"ok":true}`), ""); err != nil {
					t.Fatal(err)
				}
			}
			if event.Result != nil {
				result = event.Result
			}
		case <-deadline:
			t.Fatal("timed out waiting for Codex result")
		}
	}
	fields, _ := result.Provider.Fields.(codex.ResultFields)
	if result.Text != "hello" || result.Outcome != agent.OutcomeCompleted || result.Tokens == nil ||
		*result.Tokens.Input != 6 || *result.Tokens.Output != 5 || *result.Tokens.CacheRead != 7 || *result.Tokens.CacheCreation != 1 ||
		fields.Reason != "completed" || fields.Thread == nil || *fields.Thread.Tokens.Input != 6 ||
		result.Context == nil || *result.Context != (agent.ContextUsage{Window: 200_000, Used: 10}) {
		t.Fatalf("result = %+v", result)
	}
	if !sawPartial || !sawObservation || !sawTurnStart || !sawControl {
		t.Fatalf("events: partial=%v observation=%v turn_start=%v control=%v", sawPartial, sawObservation, sawTurnStart, sawControl)
	}
	if err := conversation.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := conversation.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeModelAndUsageDiscovery(t *testing.T) {
	root := t.TempDir()
	adapter, err := codex.New(codex.Options{
		Binary: helperBinary(t, root), ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := adapter.Models(t.Context())
	if err != nil || len(models) != 1 || models[0].ID != "gpt-test" || models[0].Name != "GPT Test" ||
		!slices.Equal(models[0].Efforts, []string{"low", "high"}) || !json.Valid(models[0].Provider.Native) {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	usage, err := adapter.Usage(t.Context())
	if err != nil || !strings.Contains(string(usage.Provider.Native), `"rate_limits"`) || !strings.Contains(string(usage.Provider.Native), `"token_usage"`) ||
		len(usage.Windows) != 1 || usage.Windows[0].ID != "codex.primary" || usage.Windows[0].UsedPercent != 42 ||
		usage.Windows[0].ResetsAt == nil || usage.Provider.Fields.(codex.SubscriptionFields).WindowMinutes["codex.primary"] != 300 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	status, err := adapter.AuthStatus(t.Context())
	if err != nil || !status.LoggedIn || status.Provider.Fields.(codex.AuthFields).Method != "chatgpt" ||
		status.Account == nil || status.Account.Email != "dev@example.test" || status.Account.Plan != "pro" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestDeviceCodeLogin(t *testing.T) {
	root := t.TempDir()
	adapter, err := codex.New(codex.Options{
		Binary: helperBinary(t, root), ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	login, err := adapter.StartLogin(t.Context(), agent.LoginRequest{Method: codex.LoginChatGPTDeviceCode})
	if err != nil {
		t.Fatal(err)
	}
	if challenge := login.Challenge(); challenge.URL != "https://auth.example/device" || challenge.UserCode != "ABCD-1234" || challenge.Flow != agent.FlowDeviceCode {
		t.Fatalf("challenge = %+v", challenge)
	}
	if err := login.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
}

func TestNativeSessionResumeAndFork(t *testing.T) {
	root := t.TempDir()
	adapter, err := codex.New(codex.Options{
		Binary: helperBinary(t, root), ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxExternal, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode, nativeID string
	}{
		{mode: agent.SessionResume, nativeID: "parent-thread"},
		{mode: agent.SessionFork, nativeID: "forked-thread"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			events := make(chan agent.Event, 16)
			request := agent.Request{
				Prompt: "continue", Model: "gpt-test", Effort: "high", WorkspaceID: strings.Repeat("b", 32),
				SessionID: "123e4567-e89b-42d3-a456-426614174000", SessionMode: test.mode,
				Permissions: agent.PermissionsAsk, WorkspaceDir: root, AddDirs: []string{root},
			}
			if test.mode == agent.SessionResume {
				request.NativeSessionID = "parent-thread"
			} else {
				request.NativeForkFrom = "parent-thread"
			}
			conversation, err := adapter.Start(t.Context(), request, func(event agent.Event) error {
				events <- event

				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if native := conversation.(agent.NativeSession).NativeSessionID(); native != test.nativeID {
				t.Fatalf("native session = %q", native)
			}
			for finished := false; !finished; {
				select {
				case event := <-events:
					if event.Permission != nil {
						if err := conversation.Answer(agent.PermissionAnswer{ID: event.Permission.ID, Allow: true}); err != nil {
							t.Fatal(err)
						}
					}
					if event.Control != nil {
						if err := conversation.Respond(event.Control.ID, json.RawMessage(`{"ok":true}`), ""); err != nil {
							t.Fatal(err)
						}
					}
					finished = event.Result != nil
				case <-time.After(3 * time.Second):
					t.Fatal("timed out waiting for resumed Codex turn")
				}
			}
			if err := conversation.CloseInput(); err != nil {
				t.Fatal(err)
			}
			if err := conversation.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A call without a workspace stays read-only even where an external boundary would otherwise
// disable the Codex sandbox.
func TestCallWithoutWorkspaceIsReadOnly(t *testing.T) {
	root := t.TempDir()
	adapter, err := codex.New(codex.Options{
		Binary: helperBinary(t, root), ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxExternal, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agent.Request{Prompt: "hi", Model: "gpt-test", Effort: "high", Options: json.RawMessage(`{"developer_instructions":"be brief"}`)}
	if err := adapter.Validate(&request); err != nil {
		t.Fatalf("validate: %v", err)
	}
	results := make(chan *agent.Result, 1)
	conversation, err := adapter.Start(t.Context(), request, func(event agent.Event) error {
		if event.Result != nil {
			results <- event.Result
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Text != "answer" || result.Outcome != agent.OutcomeCompleted {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the answer")
	}
	if err := conversation.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := conversation.Wait(); err != nil {
		t.Fatal(err)
	}
}

// An app-server that exits at startup, for example on invalid configuration, reports its own
// error instead of a broken pipe.
func TestStartupFailureReportsAppServerError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "codex-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'invalid config sk-ant-secret' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, err := codex.New(codex.Options{
		Binary: binary, ProfileDir: filepath.Join(root, "config"), HomeDir: root, User: "agent",
		WorkDir: root, TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20,
		CommandTimeout: 30 * time.Second, SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agent.Request{Prompt: "hi", Model: "gpt-test", Effort: "high"}
	if err := adapter.Validate(&request); err != nil {
		t.Fatalf("validate: %v", err)
	}
	_, err = adapter.Start(t.Context(), request, func(agent.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid config [redacted]") {
		t.Fatalf("error = %v", err)
	}
}

func helperBinary(t *testing.T, root string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	path := filepath.Join(root, "codex-app-server-test")
	script := "#!/bin/sh\nexec " + quote(executable) + " -test.run '^TestCodexAppServerHelper$' -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestCodexAppServerHelper(t *testing.T) {
	if len(os.Args) < 3 || !strings.Contains(strings.Join(os.Args, " "), "app-server --listen stdio://") {
		t.Skip("helper subprocess only")
	}
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	var approvalAnswered, controlAnswered, turnCompleted bool
	write := func(value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintln(writer, string(encoded)); err != nil {
			panic(err)
		}
		if err := writer.Flush(); err != nil {
			panic(err)
		}
	}
	var threadSandbox string
	var skillRootsSet bool
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(10)
		}
		switch message.Method {
		case "initialize":
			write(map[string]any{"id": message.ID, "result": map[string]any{}})
		case "initialized":
		case "account/login/start":
			if !strings.Contains(string(message.Params), `"type":"chatgptDeviceCode"`) {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "unexpected login type"}})
				continue
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{
				"type": "chatgptDeviceCode", "loginId": "login-1", "verificationUrl": "https://auth.example/device", "userCode": "ABCD-1234",
			}})
			write(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "other-login", "success": false, "error": "not ours"}})
			write(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login-1", "success": true}})
		case "skills/extraRoots/set":
			skillRootsSet = strings.Contains(string(message.Params), `"extraRoots":["`)
			write(map[string]any{"id": message.ID, "result": map[string]any{}})
		case "thread/start":
			var params map[string]any
			_ = json.Unmarshal(message.Params, &params)
			threadSandbox, _ = params["sandbox"].(string)
			if params["ephemeral"] == true {
				// A call without a workspace: read-only, no approvals, no MCP server.
				config, _ := params["config"].(map[string]any)
				servers, ok := config["mcp_servers"].(map[string]any)
				if threadSandbox != "read-only" || params["approvalPolicy"] != "never" || !ok || len(servers) != 0 {
					write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "call without a workspace is not read-only"}})
					continue
				}
				write(map[string]any{"id": message.ID, "result": map[string]any{"thread": map[string]string{"id": "ephemeral-thread"}}})
				continue
			}
			config, _ := params["config"].(map[string]any)
			if config["mcp_servers"] == nil || params["developerInstructions"] != "be brief" || params["approvalsReviewer"] != "user" || !skillRootsSet {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "native thread options missing"}})
				continue
			}
			if params["sandbox"] != "workspace-write" || params["approvalPolicy"] != "on-request" || params["runtimeWorkspaceRoots"] != nil {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "invalid thread policy"}})
				continue
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"thread": map[string]string{"id": "native-thread"}}})
		case "thread/resume", "thread/fork":
			var params map[string]any
			_ = json.Unmarshal(message.Params, &params)
			threadSandbox, _ = params["sandbox"].(string)
			if threadSandbox != "danger-full-access" {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "external sandbox must disable the Codex sandbox"}})
				continue
			}
			if !strings.Contains(string(message.Params), `"threadId":"parent-thread"`) {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "native thread binding missing"}})
				continue
			}
			threadID := "parent-thread"
			if message.Method == "thread/fork" {
				threadID = "forked-thread"
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"thread": map[string]string{"id": threadID}}})
		case "turn/start":
			var params struct {
				SandboxPolicy struct {
					Type          string   `json:"type"`
					WritableRoots []string `json:"writableRoots"`
					NetworkAccess any      `json:"networkAccess"`
				} `json:"sandboxPolicy"`
				Summary           string    `json:"summary"`
				DisabledPluginIDs *[]string `json:"disabledPluginIds"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if threadSandbox == "read-only" {
				if params.SandboxPolicy.Type != "readOnly" || params.SandboxPolicy.NetworkAccess != false {
					write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "invalid read-only turn"}})
					continue
				}
				write(map[string]any{"id": message.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-1"}}})
				write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "ephemeral-thread", "turn": map[string]any{
					"id": "turn-1", "status": "completed", "items": []any{map[string]string{"id": "message-1", "type": "agentMessage", "text": "answer"}},
				}}})
				continue
			}
			native := threadSandbox == "workspace-write" && params.SandboxPolicy.Type == "workspaceWrite" && len(params.SandboxPolicy.WritableRoots) == 1
			external := threadSandbox == "danger-full-access" && params.SandboxPolicy.Type == "externalSandbox" && params.SandboxPolicy.NetworkAccess == "enabled"
			if threadSandbox == "workspace-write" && (params.Summary != "concise" || params.DisabledPluginIDs == nil) {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "native turn options missing"}})
				continue
			}
			if !native && !external {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32602, "message": "invalid turn sandbox"}})
				continue
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-1"}}})
			write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "turn-1", "items": []any{}, "status": "inProgress"}}})
			write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "native-thread", "turnId": "turn-1", "itemId": "message-1", "delta": "hel"}})
			write(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "native-thread", "turnId": "turn-1", "tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 4, "outputTokens": 3, "cachedInputTokens": 2, "cacheWriteInputTokens": 1}, "total": map[string]int{"inputTokens": 4, "outputTokens": 3, "cachedInputTokens": 2, "cacheWriteInputTokens": 1}, "modelContextWindow": 200000}}})
			// A second model call of the same turn (after a tool call).
			write(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "native-thread", "turnId": "turn-1", "tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 10, "outputTokens": 2, "cachedInputTokens": 5}, "total": map[string]int{"inputTokens": 14, "outputTokens": 5, "cachedInputTokens": 7, "cacheWriteInputTokens": 1}, "modelContextWindow": 200000}}})
			write(map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]string{"id": "command-1", "type": "commandExecution"}}})
			write(map[string]any{"method": "item/commandExecution/outputDelta", "params": map[string]any{"threadId": "native-thread", "turnId": "turn-1", "itemId": "command-1", "delta": "out"}})
			write(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]string{"id": "command-1", "type": "commandExecution"}}})
			write(map[string]any{"id": "approval-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "native-thread", "turnId": "turn-1", "itemId": "command-1", "startedAtMs": 1, "command": "pwd"}})
			write(map[string]any{"id": "control-1", "method": "item/tool/call", "params": map[string]any{"tool": "client-tool", "arguments": map[string]any{"value": 1}}})
		case "skills/list":
			write(map[string]any{"id": message.ID, "result": map[string]any{"data": []any{map[string]string{"id": "skill-1"}}}})
		case "model/list":
			write(map[string]any{"id": message.ID, "result": map[string]any{
				"data": []any{map[string]any{
					"id": "model-1", "model": "gpt-test", "displayName": "GPT Test",
					"supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "low"}, map[string]string{"reasoningEffort": "high"}},
				}},
				"nextCursor": nil,
			}})
		case "account/rateLimits/read":
			write(map[string]any{"id": message.ID, "result": map[string]any{
				"rateLimits": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 42, "resetsAt": 1_800_000_000, "windowDurationMins": 300}},
			}})
		case "account/read":
			write(map[string]any{"id": message.ID, "result": map[string]any{
				"account": map[string]any{"type": "chatgpt", "email": "dev@example.test", "planType": "pro"},
			}})
		case "account/usage/read":
			write(map[string]any{"id": message.ID, "result": map[string]any{"summary": map[string]any{"inputTokens": 4}}})
		case "":
			switch string(message.ID) {
			case `"approval-1"`:
				if !strings.Contains(string(scanner.Bytes()), `"decision":"accept"`) {
					os.Exit(11)
				}
				approvalAnswered = true
			case `"control-1"`:
				if !strings.Contains(string(scanner.Bytes()), `"ok":true`) {
					os.Exit(13)
				}
				controlAnswered = true
			default:
				os.Exit(14)
			}
			if approvalAnswered && controlAnswered && !turnCompleted {
				turnCompleted = true
				write(map[string]any{
					"method": "turn/completed",
					"params": map[string]any{
						"threadId": "native-thread",
						"turn": map[string]any{
							"id": "turn-1", "status": "completed",
							"items": []any{map[string]string{"id": "message-1", "type": "agentMessage", "text": "hello"}},
						},
					},
				})
			}
		default:
			write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "unexpected method"}})
		}
	}
	if err := scanner.Err(); err != nil {
		os.Exit(12)
	}
	os.Exit(0)
}
