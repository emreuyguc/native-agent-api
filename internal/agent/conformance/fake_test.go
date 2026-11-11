package conformance_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent/agenttest"
)

// The fake native CLIs run as this test binary (see fakeBinary). Both answer every prompt
// with "<mode>|<decision>|<prompt>": mode is how the conversation was opened (stateless, new,
// resume, fork), decision the permission answer (none, allow, deny). Conversations are files
// in FAKE_STATE, so resuming or forking an unknown native conversation fails like the real CLI.

const fakeModel = "test-model"

// fakeBinary writes an executable that runs the fake CLI of the given provider.
func fakeBinary(t *testing.T, provider string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, provider)
	script := "#!/bin/sh\nFAKE_PROVIDER=" + provider + " FAKE_STATE=" + quote(state) +
		" exec " + quote(executable) + " -test.run '^TestFakeNativeCLI$' -- \"$@\"\n"
	agenttest.WriteExecutable(t, path, script)

	return path
}

// TestFakeNativeCLI is the fake CLI process; it is skipped in normal test runs.
func TestFakeNativeCLI(t *testing.T) {
	provider := os.Getenv("FAKE_PROVIDER")
	if provider == "" {
		t.Skip("fake native CLI subprocess only")
	}
	args := os.Args
	if index := slices.Index(args, "--"); index >= 0 {
		args = args[index+1:]
	}
	fake := &fakeCLI{state: os.Getenv("FAKE_STATE"), out: bufio.NewWriter(os.Stdout), in: bufio.NewScanner(os.Stdin)}
	fake.in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	code := 0
	if err := map[string]func([]string) error{"claude": fake.claude, "codex": fake.codex}[provider](args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	_ = fake.out.Flush()
	os.Exit(code) // Never let the test framework print to the protocol stream.
}

type fakeCLI struct {
	state string
	out   *bufio.Writer
	in    *bufio.Scanner
}

func (f *fakeCLI) write(value any) {
	encoded, _ := json.Marshal(value)
	_, _ = f.out.Write(append(encoded, '\n'))
	_ = f.out.Flush()
}

// open records or checks native conversations. It returns the conversation's native ID.
func (f *fakeCLI) open(mode, id, parent string) (string, error) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(f.state, name))
		return name != "" && err == nil
	}
	switch mode {
	case "resume":
		if !exists(id) {
			return "", fmt.Errorf("no conversation found with session ID: %s", id)
		}
		return id, nil
	case "fork":
		if !exists(parent) {
			return "", fmt.Errorf("no conversation to fork: %s", parent)
		}
	}
	if mode == "stateless" {
		return "", nil
	}

	return id, os.WriteFile(filepath.Join(f.state, id), []byte(mode), 0o600)
}

func answer(mode, decision, prompt string) string { return mode + "|" + decision + "|" + prompt }

func flagValue(args []string, flag string) (string, bool) {
	index := slices.Index(args, flag)
	if index < 0 || index+1 >= len(args) {
		return "", false
	}

	return args[index+1], true
}

var fakeClaudeModels = []map[string]any{{"value": fakeModel, "displayName": "Test Model", "supportsEffort": true, "supportedEffortLevels": []string{"low", "high"}}}

// claude emulates `claude -p --input-format stream-json --output-format stream-json`.
func (f *fakeCLI) claude(args []string) error {
	if mode, ok := flagValue(args, "--permission-mode"); ok && mode == "" {
		return errors.New("error: option '--permission-mode <mode>' argument '' is invalid")
	}
	if model, ok := flagValue(args, "--model"); ok && model != fakeModel {
		return fmt.Errorf("unknown model %q", model)
	}
	mode, id, parent := "new", "", ""
	sessionID, _ := flagValue(args, "--session-id")
	resume, resuming := flagValue(args, "--resume")
	switch {
	case slices.Contains(args, "--no-session-persistence"):
		mode = "stateless"
	case slices.Contains(args, "--fork-session"):
		mode, id, parent = "fork", sessionID, resume
	case resuming:
		mode, id = "resume", resume
	default:
		id = sessionID
	}
	opened := false
	ask := slices.Contains(args, "--permission-prompt-tool")
	permissions := 0
	for f.in.Scan() {
		var message struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(f.in.Bytes(), &message) != nil {
			return fmt.Errorf("invalid stdin line %q", f.in.Text())
		}
		switch {
		case message.Type == "control_request" && message.Request.Subtype == "initialize":
			f.write(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": message.RequestID, "response": map[string]any{"models": fakeClaudeModels},
			}})
		case message.Type == "user":
			if !opened {
				if _, err := f.open(mode, id, parent); err != nil {
					f.write(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error()})
					return err
				}
				opened = true
				f.write(map[string]any{"type": "system", "subtype": "init", "session_id": id})
			}
			decision := "none"
			if ask {
				permissions++
				requestID := fmt.Sprintf("perm-%d", permissions)
				f.write(map[string]any{"type": "control_request", "request_id": requestID, "request": map[string]any{
					"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": "a.txt"},
				}})
				if !f.in.Scan() {
					return nil
				}
				decision = "deny"
				if strings.Contains(f.in.Text(), `"behavior":"allow"`) {
					decision = "allow"
				}
			}
			f.write(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}})
			f.write(map[string]any{
				"type": "result", "subtype": "success", "is_error": false, "session_id": id,
				"result": answer(mode, decision, message.Message.Content),
			})
		}
	}

	return nil
}

// codex emulates `codex app-server --listen stdio://`.
func (f *fakeCLI) codex(args []string) error {
	if !slices.Contains(args, "app-server") {
		return fmt.Errorf("unexpected arguments %v", args)
	}
	var mode, threadID string
	var ask bool
	threads, turns := 0, 0
	pending := map[string]string{} // approval request ID → prompt
	completeTurn := func(turnID, decision, prompt string) {
		f.write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{
			"id": turnID, "status": "completed", "items": []any{map[string]string{"id": "msg-" + turnID, "type": "agentMessage", "text": answer(mode, decision, prompt)}},
		}}})
	}
	for f.in.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result map[string]any  `json:"result"`
		}
		if json.Unmarshal(f.in.Bytes(), &message) != nil {
			return fmt.Errorf("invalid stdin line %q", f.in.Text())
		}
		reply := func(result any) { f.write(map[string]any{"id": message.ID, "result": result}) }
		fail := func(err error) {
			f.write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32600, "message": err.Error()}})
		}
		switch message.Method {
		case "initialize":
			reply(map[string]any{})
		case "initialized":
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{
				"id": "m1", "model": fakeModel, "displayName": "Test Model",
				"supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "low"}, map[string]string{"reasoningEffort": "high"}},
			}}, "nextCursor": nil})
		case "thread/start", "thread/resume", "thread/fork":
			// The real app-server records an explicitly requested cwd as a trusted project in the
			// user's native profile (docs/security.md F-8).
			if _, ok := message.Params["cwd"]; ok {
				fail(errors.New("cwd would mark the project trusted in the native profile"))
				continue
			}
			if message.Params["model"] != fakeModel {
				fail(fmt.Errorf("unknown model %v", message.Params["model"]))
				continue
			}
			ask = message.Params["approvalPolicy"] == "on-request"
			threads++
			parent, _ := message.Params["threadId"].(string)
			var err error
			switch message.Method {
			case "thread/resume":
				mode = "resume"
				threadID, err = f.open(mode, parent, "")
			case "thread/fork":
				mode = "fork"
				threadID, err = f.open(mode, fmt.Sprintf("thread-%d-%d", threads, os.Getpid()), parent)
			default:
				mode = "new"
				if message.Params["ephemeral"] == true {
					mode = "stateless"
				}
				threadID = fmt.Sprintf("thread-%d-%d", threads, os.Getpid())
				_, err = f.open(mode, threadID, "")
			}
			if err != nil {
				fail(err)
				continue
			}
			// Like the real app-server, notify before answering: the gateway must keep reading
			// while it waits for the response.
			f.write(map[string]any{"method": "thread/started", "params": map[string]any{"thread": map[string]string{"id": threadID}}})
			reply(map[string]any{"thread": map[string]string{"id": threadID}})
		case "turn/start":
			// Like the real app-server, a list field may be empty but never null.
			if policy, ok := message.Params["sandboxPolicy"].(map[string]any); ok {
				if roots, present := policy["writableRoots"]; present && roots == nil {
					fail(errors.New("Invalid request: invalid type: null, expected a sequence"))
					continue
				}
			}
			turns++
			turnID := fmt.Sprintf("turn-%d", turns)
			prompt := ""
			if input, ok := message.Params["input"].([]any); ok && len(input) > 0 {
				prompt, _ = input[0].(map[string]any)["text"].(string)
			}
			reply(map[string]any{"turn": map[string]string{"id": turnID}})
			f.write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "inProgress"}}})
			if !ask {
				completeTurn(turnID, "none", prompt)
				continue
			}
			requestID := "approval-" + turnID
			pending[requestID] = prompt
			f.write(map[string]any{"id": requestID, "method": "item/commandExecution/requestApproval", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "itemId": "cmd-" + turnID, "command": "touch a.txt",
			}})
		case "":
			var requestID string
			_ = json.Unmarshal(message.ID, &requestID)
			prompt, ok := pending[requestID]
			if !ok {
				return fmt.Errorf("response to unknown request %s", message.ID)
			}
			delete(pending, requestID)
			decision := "deny"
			if message.Result["decision"] == "accept" {
				decision = "allow"
			}
			completeTurn(strings.TrimPrefix(requestID, "approval-"), decision, prompt)
		default:
			fail(fmt.Errorf("method %s is not supported by the fake", message.Method))
		}
	}

	return nil
}
