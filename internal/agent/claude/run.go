package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/proctree"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// Errors returned by Conversation.Wait.
var (
	ErrOutputLimit   = errors.New("claude output exceeded the configured limit")
	ErrInvalidStream = errors.New("claude returned invalid stream data")
	ErrNoResult      = errors.New("claude exited without returning a result")
)

// startupExitGrace is how long a CLI that closed its input at startup may take to exit and
// report why before the gateway stops it.
const startupExitGrace = 5 * time.Second

// Start implements agent.Agent. It runs `claude -p --input-format stream-json` and keeps
// the process open: user messages, interrupts and permission answers are written to stdin
// using Claude Code's stream-json and control protocol.
func (a *Agent) Start(ctx context.Context, req agent.Request, onEvent agent.EventFunc) (agent.Conversation, error) {
	scratch, err := os.MkdirTemp(a.opts.TempDir, "claude-run-")
	if err != nil {
		return nil, fmt.Errorf("create run scratch directory: %w", err)
	}
	options, err := ParseOptions(&req)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	args, err := a.prepare(req, options, scratch)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	cmd := a.command(ctx, args...)
	cmd.Dir = req.WorkspaceDir
	if cmd.Dir == "" {
		cmd.Dir = scratch
	}
	cmd.Env = RunEnvironment(a.opts, options)
	stderr := iox.NewTailBuffer(32_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		cancel()
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("open claude pipes: %w", err)
	}

	log := a.opts.Logger.With("run_id", req.RunID, "workspace_id", req.WorkspaceID, "session_id", req.SessionID, "session_mode", req.SessionMode)
	log.DebugContext(ctx, "starting agent process", "args", args, "dir", cmd.Dir)
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(scratch)
		log.ErrorContext(ctx, "agent process failed to start", "binary", a.opts.Binary, "err", err)
		return nil, fmt.Errorf("start claude: %w", err)
	}
	log.InfoContext(ctx, "agent process started", "pid", cmd.Process.Pid, "model", req.Model,
		"stateless", req.Stateless(), "permissions", req.Permissions)

	c := &conversation{
		stdin:           stdin,
		nativeSessionID: req.SessionID,
		pending:         make(map[string]json.RawMessage),
		pendingControl:  make(map[string]struct{}),
		waiters:         make(map[string]chan controlResult),
		done:            make(chan struct{}),
		log:             log,
	}
	var (
		sendErr   error
		exitTimer *time.Timer
	)
	if options.Initialize != nil {
		// Sent before the first message, as the Agent SDK does; the response is forwarded
		// to clients as a native control_response event.
		request := maps.Clone(options.Initialize)
		request["subtype"] = "initialize"
		sendErr = c.write(map[string]any{"type": "control_request", "request_id": "gateway-initialize", "request": request})
	}
	if sendErr == nil {
		sendErr = c.Send(req.Prompt)
	}
	if sendErr != nil {
		// The CLI closed its input, usually because it exited at startup. Its output and stderr
		// explain why, so the consumer below reports the failure. The timer stops a CLI that
		// keeps running without input.
		exitTimer = time.AfterFunc(startupExitGrace, cancel)
	}

	go func() {
		defer close(c.done)
		defer os.RemoveAll(scratch)
		defer cancel()
		started := time.Now()
		turns, events, streamErr := c.consume(stdout, a.opts.MaxOutputBytes, onEvent)
		c.failWaiters()
		if streamErr != nil {
			cancel() // Kills the process group so Wait does not block on a misbehaving CLI.
		}
		waitErr := cmd.Wait()
		if exitTimer != nil {
			exitTimer.Stop()
		}
		proctree.Reap(cmd.Process.Pid)
		if options.Debug {
			debugOutput := redact.Message(stderr.String(), 32_000)
			if debugOutput != "" {
				if err := onEvent(agent.Event{Diagnostic: debugOutput}); streamErr == nil {
					streamErr = err
				}
			}
		}
		attrs := []any{"duration", time.Since(started), "turns", turns, "events", events, "exit_code", cmd.ProcessState.ExitCode()}
		switch {
		case parent.Err() != nil:
			log.WarnContext(ctx, "agent process stopped", append(attrs, "cause", context.Cause(parent))...)
			c.err = fmt.Errorf("claude run cancelled: %w", context.Cause(parent))
		case streamErr != nil:
			log.ErrorContext(ctx, "agent process failed", append(attrs, "err", streamErr)...)
			c.err = streamErr
		case waitErr != nil && turns == 0:
			message := redact.Message(stderr.String(), maxErrorMessage)
			switch {
			case message != "":
			case sendErr != nil:
				message = "send prompt: " + sendErr.Error()
			default:
				message = waitErr.Error()
			}
			log.ErrorContext(ctx, "agent process failed", append(attrs, "err", message)...)
			c.err = errors.New(message)
		case turns == 0 && sendErr != nil:
			log.ErrorContext(ctx, "agent process failed", append(attrs, "err", sendErr)...)
			c.err = fmt.Errorf("send prompt: %w", sendErr)
		case turns == 0:
			log.ErrorContext(ctx, "agent process failed", append(attrs, "err", ErrNoResult)...)
			c.err = ErrNoResult
		default:
			// A non-zero exit after completed turns reflects the last turn's error result,
			// which the caller already received.
			log.InfoContext(ctx, "agent process finished", attrs...)
		}
	}()

	return c, nil
}

// prepare writes per-run files and returns the CLI arguments.
func (a *Agent) prepare(req agent.Request, options RequestOptions, scratch string) ([]string, error) {
	files := Files{EmptyMCP: filepath.Join(scratch, "mcp.json")}
	if err := os.WriteFile(files.EmptyMCP, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		return nil, fmt.Errorf("write MCP policy: %w", err)
	}
	if options.MCPConfig != nil {
		encoded, err := json.Marshal(options.MCPConfig)
		if err != nil {
			return nil, fmt.Errorf("encode MCP config: %w", err)
		}
		files.ClientMCP = filepath.Join(scratch, "client-mcp.json")
		if err := os.WriteFile(files.ClientMCP, encoded, 0o600); err != nil {
			return nil, fmt.Errorf("write MCP config: %w", err)
		}
	}
	args := Arguments(req, options, a.opts.SandboxMode, files)
	if req.SystemPrompt != "" {
		systemPath := filepath.Join(scratch, "system.txt")
		if err := os.WriteFile(systemPath, []byte(req.SystemPrompt), 0o600); err != nil {
			return nil, fmt.Errorf("write system prompt: %w", err)
		}
		args = append(args, "--system-prompt-file", systemPath)
	}
	if req.OutputSchema != nil {
		schema, err := json.Marshal(req.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode output schema: %w", err)
		}
		args = append(args, "--json-schema", string(schema))
	}

	return args, nil
}

// conversation is a running claude process.
type conversation struct {
	log             *slog.Logger
	done            chan struct{}
	err             error
	control         atomic.Int64
	nativeSessionID string
	// interrupted is set by Interrupt and cleared by the next result, so that result reports
	// the turn as interrupted rather than failed.
	interrupted atomic.Bool
	// Read and written only by consume: the model and input size of the main agent's latest
	// model call.
	lastModel  string
	lastPrompt *int64

	mu             sync.Mutex
	stdin          io.WriteCloser
	closed         bool
	pending        map[string]json.RawMessage    // permission request id → original tool input
	pendingControl map[string]struct{}           // other agent → client control requests
	waiters        map[string]chan controlResult // client → agent control requests
}

type controlResult struct {
	response json.RawMessage
	err      error
}

// errConversationEnded fails control requests still waiting when the process exits.
var errConversationEnded = errors.New("claude process ended before answering")

var _ agent.Conversation = (*conversation)(nil)

// NativeSessionID implements agent.NativeSession.
func (c *conversation) NativeSessionID() string { return c.nativeSessionID }

// write sends one JSON line to the CLI.
func (c *conversation) write(message any) error {
	line, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return agent.ErrInputClosed
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write to claude: %w", err)
	}

	return nil
}

// Send implements agent.Conversation.
func (c *conversation) Send(text string) error {
	return c.write(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
}

// Interrupt implements agent.Conversation.
func (c *conversation) Interrupt() error {
	c.interrupted.Store(true)
	return c.write(map[string]any{
		"type":       "control_request",
		"request_id": "gateway-" + strconv.FormatInt(c.control.Add(1), 10),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// Answer implements agent.Conversation.
func (c *conversation) Answer(answer agent.PermissionAnswer) error {
	c.mu.Lock()
	input, ok := c.pending[answer.ID]
	delete(c.pending, answer.ID)
	c.mu.Unlock()
	if !ok {
		return agent.ErrUnknownPermission
	}
	var options AllowOptions
	if err := agent.DecodeStrict(answer.Options, "provider", &options); err != nil {
		return err
	}
	if !answer.Allow && options.UpdatedInput != nil {
		return agent.Invalid("provider.updated_input is only allowed with allow")
	}
	message := answer.Message
	if message == "" {
		message = "The user denied this action."
	}
	decision := map[string]any{"behavior": "deny", "message": message}
	if answer.Allow {
		var updated any = input
		if options.UpdatedInput != nil {
			updated = options.UpdatedInput
		}
		decision = map[string]any{"behavior": "allow", "updatedInput": updated}
	}

	return c.write(controlResponse(answer.ID, decision))
}

// Control implements agent.Conversation.
func (c *conversation) Control(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	var header struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(request, &header); err != nil || header.Subtype == "" {
		return nil, agent.Invalid("control request must be a JSON object with a subtype")
	}
	id := "gateway-" + strconv.FormatInt(c.control.Add(1), 10)
	result := make(chan controlResult, 1)
	c.mu.Lock()
	c.waiters[id] = result
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()
	if err := c.writeControl(id, request); err != nil {
		return nil, err
	}
	select {
	case r := <-result:
		return r.response, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// writeControl writes a control request even after CloseInput: control does not need
// the input to stay open while the current turn runs.
func (c *conversation) writeControl(id string, request json.RawMessage) error {
	line, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return agent.ErrInputClosed
	}
	_, err = c.stdin.Write(append(line, '\n'))

	return err
}

// Respond implements agent.Conversation.
func (c *conversation) Respond(id string, response json.RawMessage, errMessage string) error {
	c.mu.Lock()
	_, ok := c.pendingControl[id]
	delete(c.pendingControl, id)
	c.mu.Unlock()
	if !ok {
		return agent.ErrUnknownControl
	}
	if errMessage != "" {
		return c.write(map[string]any{
			"type":     "control_response",
			"response": map[string]any{"subtype": "error", "request_id": id, "error": errMessage},
		})
	}
	if len(response) == 0 {
		response = json.RawMessage(`{}`)
	}

	return c.write(controlResponse(id, response))
}

func (c *conversation) failWaiters() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, waiter := range c.waiters {
		waiter <- controlResult{err: errConversationEnded}
		delete(c.waiters, id)
	}
}

// CloseInput implements agent.Conversation.
func (c *conversation) CloseInput() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	return c.stdin.Close()
}

// Wait implements agent.Conversation.
func (c *conversation) Wait() error {
	<-c.done
	return c.err
}

func controlResponse(requestID string, response any) map[string]any {
	return map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": requestID, "response": response},
	}
}

// nativeEvent holds the fields of a stream-json line that the adapter interprets.
type nativeEvent struct {
	Type       string                      `json:"type"`
	Subtype    string                      `json:"subtype"`
	IsError    *bool                       `json:"is_error"`
	Result     string                      `json:"result"`
	RequestID  string                      `json:"request_id"`
	Request    json.RawMessage             `json:"request"`
	Usage      *nativeTokenUsage           `json:"usage"`
	ModelUsage map[string]nativeModelUsage `json:"modelUsage"`
	// ModelUsageSinceLastResult is modelUsage since the previous result: this turn, also for
	// the first turn after a resume.
	ModelUsageSinceLastResult map[string]nativeModelUsage `json:"model_usage_since_last_result"`
	TotalCostUSD              *float64                    `json:"total_cost_usd"`
	// Message and ParentToolUseID are set on assistant events; a subagent's messages have a
	// parent tool use.
	Message *struct {
		Model string            `json:"model"`
		Usage *nativeTokenUsage `json:"usage"`
	} `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	// Origin is present on a turn the CLI did not start for a user message, e.g.
	// {"kind":"task-notification"}; human turns omit it or report kind "human".
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

type nativeTokenUsage struct {
	InputTokens         *int64 `json:"input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
}

type nativeModelUsage struct {
	InputTokens         *int64   `json:"inputTokens"`
	OutputTokens        *int64   `json:"outputTokens"`
	CacheReadTokens     *int64   `json:"cacheReadInputTokens"`
	CacheCreationTokens *int64   `json:"cacheCreationInputTokens"`
	CostUSD             *float64 `json:"costUSD"`
	ContextWindow       *int64   `json:"contextWindow"`
	MaxOutputTokens     *int64   `json:"maxOutputTokens"`
}

// toolRequest is the body of a can_use_tool control request.
type toolRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
}

// consume reads the CLI output until EOF. limit bounds the stored output of one turn;
// partial messages are not stored and do not count.
func (c *conversation) consume(stdout io.Reader, limit int64, onEvent agent.EventFunc) (turns, events int, err error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), int(limit))
	var turnBytes int64
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var native nativeEvent
		if err := json.Unmarshal(line, &native); err != nil {
			return turns, events, ErrInvalidStream
		}
		event := agent.Event{Type: native.Type, Raw: bytes.Clone(line), Partial: native.Type == "stream_event"}
		if !event.Partial {
			if turnBytes += int64(len(line)); turnBytes > limit {
				return turns, events, ErrOutputLimit
			}
		}
		switch native.Type {
		case "control_response":
			if c.deliver(native.Response.RequestID, native.Response.Subtype, native.Response.Response, native.Response.Error) {
				continue
			}
		case "control_request":
			if err := c.classifyControl(&event, native); err != nil {
				return turns, events, err
			}
		case "assistant":
			c.recordCall(native)
		case "system":
			event.TurnStarted = native.Subtype == "init"
			event.Observation = taskObservation(native.Subtype, line)
		case "result":
			turns++
			turnBytes = 0
			agentInitiated := native.Origin != nil && native.Origin.Kind != "" && native.Origin.Kind != "human"
			interrupted := !agentInitiated && c.interrupted.Swap(false)
			result := outcome(native.Subtype, native.IsError == nil || *native.IsError, interrupted)
			text, failure := native.Result, ""
			if result != agent.OutcomeCompleted {
				// A failed result carries the error description instead of an answer.
				text, failure = "", redact.Message(native.Result, maxErrorMessage)
				if failure == "" {
					failure = "claude result " + native.Subtype
				}
			}
			fields := resultFields(native)
			event.Result = &agent.Result{
				Text: text, Outcome: result, Error: failure, Tokens: turnTokens(native), Context: c.contextUsage(fields.Session.Models),
				Provider:       agent.ResultProvider{Provider: agent.NewProvider(Name, fields, event.Raw)},
				AgentInitiated: agentInitiated,
			}
			c.lastModel, c.lastPrompt = "", nil
		}
		events++
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return turns, events, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return turns, events, fmt.Errorf("read claude stream: %w", err)
	}

	return turns, events, nil
}

func turnTokens(native nativeEvent) *agent.Tokens {
	if native.Usage == nil {
		return nil
	}

	return &agent.Tokens{
		Input: native.Usage.InputTokens, Output: native.Usage.OutputTokens,
		CacheRead: native.Usage.CacheReadTokens, CacheCreation: native.Usage.CacheCreationTokens,
	}
}

// recordCall remembers the main agent's latest model call; subagent calls do not fill its
// context.
func (c *conversation) recordCall(native nativeEvent) {
	if native.ParentToolUseID != nil && *native.ParentToolUseID != "" || native.Message == nil || native.Message.Usage == nil {
		return
	}
	usage := native.Message.Usage
	var prompt int64
	for _, count := range []*int64{usage.InputTokens, usage.CacheReadTokens, usage.CacheCreationTokens} {
		if count != nil {
			prompt += *count
		}
	}
	c.lastModel, c.lastPrompt = native.Message.Model, &prompt
}

// contextUsage is the main agent's latest call against its model's context window. The
// window comes from modelUsage: the call's model, or the only model reported.
func (c *conversation) contextUsage(models map[string]ModelUsage) *agent.ContextUsage {
	if c.lastPrompt == nil {
		return nil
	}
	usage, ok := models[c.lastModel]
	if !ok && len(models) == 1 {
		for _, only := range models {
			usage, ok = only, true
		}
	}
	if !ok || usage.ContextWindow == nil {
		return nil
	}

	return &agent.ContextUsage{Window: *usage.ContextWindow, Used: *c.lastPrompt}
}

func resultFields(native nativeEvent) ResultFields {
	fields := ResultFields{Reason: native.Subtype, Session: Usage{CostUSD: native.TotalCostUSD, Models: modelUsage(native.ModelUsage)}}
	if native.ModelUsageSinceLastResult != nil {
		turn := Usage{Models: modelUsage(native.ModelUsageSinceLastResult)}
		for _, usage := range native.ModelUsageSinceLastResult {
			if usage.CostUSD != nil {
				cost := *usage.CostUSD
				if turn.CostUSD != nil {
					cost += *turn.CostUSD
				}
				turn.CostUSD = &cost
			}
		}
		fields.Turn = &turn
	}

	return fields
}

func modelUsage(native map[string]nativeModelUsage) map[string]ModelUsage {
	if len(native) == 0 {
		return nil
	}
	models := make(map[string]ModelUsage, len(native))
	for model, usage := range native {
		models[model] = ModelUsage{
			Tokens: agent.Tokens{
				Input: usage.InputTokens, Output: usage.OutputTokens,
				CacheRead: usage.CacheReadTokens, CacheCreation: usage.CacheCreationTokens,
			},
			CostUSD: usage.CostUSD, ContextWindow: usage.ContextWindow, MaxOutputTokens: usage.MaxOutputTokens,
		}
	}

	return models
}

// deliver routes a control response to the waiting Control call. Responses nobody waits
// for (initialize, interrupt) are forwarded as native events.
func (c *conversation) deliver(id, subtype string, response json.RawMessage, message string) bool {
	c.mu.Lock()
	waiter, ok := c.waiters[id]
	delete(c.waiters, id)
	c.mu.Unlock()
	if !ok {
		return false
	}
	if subtype == "error" {
		waiter <- controlResult{err: fmt.Errorf("%w: %s", agent.ErrControlRejected, redact.Message(message, maxErrorMessage))}
		return true
	}
	if len(response) == 0 {
		response = json.RawMessage(`null`)
	}
	waiter <- controlResult{response: bytes.Clone(response)}

	return true
}

// classifyControl turns a control request from the CLI into a permission request (tool
// use) or a generic control request the client answers with Respond.
func (c *conversation) classifyControl(event *agent.Event, native nativeEvent) error {
	var tool toolRequest
	if err := json.Unmarshal(native.Request, &tool); err != nil || native.RequestID == "" {
		return ErrInvalidStream
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if tool.Subtype != "can_use_tool" {
		c.pendingControl[native.RequestID] = struct{}{}
		event.Control = &agent.ControlRequest{ID: native.RequestID, Provider: agent.ControlProvider{
			Provider: agent.NewProvider(Name, ControlFields{Subtype: tool.Subtype}, bytes.Clone(native.Request)),
		}}

		return nil
	}
	input := tool.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	c.pending[native.RequestID] = input
	event.Permission = &agent.PermissionRequest{
		ID: native.RequestID, Kind: permissionKind(tool.ToolName), Summary: permissionSummary(tool.ToolName, tool.Description, input),
		Provider: agent.PermissionProvider{
			Provider: agent.NewProvider(Name, PermissionFields{Tool: tool.ToolName}, bytes.Clone(native.Request)),
		},
	}

	return nil
}
