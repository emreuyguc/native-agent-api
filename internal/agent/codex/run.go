package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/proctree"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

var (
	errInvalidStream = errors.New("codex returned invalid app-server data")
	errOutputLimit   = errors.New("codex output exceeded the configured limit")
	errProcessEnded  = errors.New("codex app-server ended before answering")
)

// startupExitGrace is how long an app-server that stopped answering at startup may take to
// exit and report why before the gateway stops it.
const startupExitGrace = 5 * time.Second

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type pendingRequest struct {
	id     json.RawMessage
	method string
}

type conversation struct {
	ctx     context.Context // Run-lifetime context; cancellation terminates every pending RPC.
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	done    chan struct{}
	scratch string

	onEvent        agent.EventFunc
	maxOutputBytes int64
	sandbox        agent.SandboxMode

	next atomic.Int64

	mu       sync.Mutex
	closed   bool
	err      error
	threadID string
	turnID   string
	request  agent.Request
	options  RequestOptions
	waiters  map[string]chan rpcResult
	pending  map[string]pendingRequest
	text     strings.Builder
	tokens   *nativeTokens // The turn's model calls so far.
	thread   *ThreadUsage
	context  *agent.ContextUsage // After the turn's latest model call.
}

var (
	_ agent.Conversation  = (*conversation)(nil)
	_ agent.NativeSession = (*conversation)(nil)
)

func (a *Agent) Start(parent context.Context, req agent.Request, onEvent agent.EventFunc) (agent.Conversation, error) {
	options, err := ParseOptions(&req)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(a.opts.TempDir, "codex-run-")
	if err != nil {
		return nil, fmt.Errorf("create codex run scratch directory: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := commandProcess(ctx, a.opts.Binary, "app-server", "--listen", "stdio://")
	cmd.Dir = req.WorkspaceDir
	if cmd.Dir == "" {
		cmd.Dir = scratch
	}
	cmd.Env = Environment(a.opts)
	stderr := iox.NewTailBuffer(32_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		cancel()
		_ = os.RemoveAll(scratch)

		return nil, fmt.Errorf("open codex app-server pipes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(scratch)

		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	c := &conversation{
		ctx: ctx, cancel: cancel, stdin: stdin, done: make(chan struct{}), scratch: scratch,
		onEvent: onEvent, maxOutputBytes: a.opts.MaxOutputBytes, sandbox: a.opts.SandboxMode,
		request: req, options: options, waiters: make(map[string]chan rpcResult), pending: make(map[string]pendingRequest),
	}
	go c.consume(cmd, stdout, stderr)
	if _, err := c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "native_agent_api", "title": "Native Agent API", "version": "0",
	}}); err != nil {
		return nil, fmt.Errorf("initialize codex app-server: %w", c.startFailed(err))
	}
	if err := c.notify("initialized", nil); err != nil {
		return nil, fmt.Errorf("acknowledge codex app-server initialization: %w", c.startFailed(err))
	}
	if len(options.SkillRoots) > 0 {
		if _, err := c.call(ctx, "skills/extraRoots/set", map[string]any{"extraRoots": options.SkillRoots}); err != nil {
			return nil, fmt.Errorf("skills/extraRoots/set: %w", c.startFailed(err))
		}
	}
	if err := c.openThread(ctx); err != nil {
		return nil, c.startFailed(err)
	}
	if err := c.startTurn(ctx, req.Prompt); err != nil {
		return nil, c.startFailed(err)
	}

	return c, nil
}

func (c *conversation) NativeSessionID() string {
	if c.request.Stateless() {
		return ""
	}

	return c.threadID
}

func (c *conversation) openThread(ctx context.Context) error {
	// No "cwd": the app-server runs in the working directory already, and an explicit cwd makes
	// thread/start record the project as trusted in the native profile. Trust then loads
	// project config such as .codex/config.toml MCP servers, which the agent can write itself
	// and which start outside the CLI sandbox (docs/security.md F-7, F-8). An undecided project
	// keeps AGENTS.md but not project config, hooks or exec policies.
	params := map[string]any{
		"model":          c.request.Model,
		"approvalPolicy": approvalPolicy(c.request), "sandbox": sandboxPolicy(c.request, c.sandbox),
	}
	if c.request.SystemPrompt != "" {
		params["baseInstructions"] = c.request.SystemPrompt
	}
	if c.request.Stateless() {
		params["ephemeral"] = true
	}
	c.options.threadParams(params)
	method := "thread/start"
	switch c.request.SessionMode {
	case agent.SessionResume:
		method, params["threadId"], params["excludeTurns"] = "thread/resume", c.request.NativeSessionID, true
	case agent.SessionFork:
		method, params["threadId"], params["excludeTurns"] = "thread/fork", c.request.NativeForkFrom, true
	default:
		params["ephemeral"] = c.request.Stateless()
	}
	result, err := c.call(ctx, method, params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.Thread.ID == "" {
		return errors.New("codex app-server returned no thread id")
	}
	c.threadID = response.Thread.ID

	return nil
}

func approvalPolicy(req agent.Request) string {
	if req.Permissions == agent.PermissionsAsk {
		return "on-request"
	}

	return "never"
}

// sandboxPolicy is the thread-level sandbox mode. External isolation maps to
// danger-full-access because Codex must not try to start its own sandbox inside it.
func sandboxPolicy(req agent.Request, sandbox agent.SandboxMode) string {
	// A call without a workspace is always read-only, also inside an external boundary: there
	// Codex cannot start its own sandbox, so any command attempt fails closed.
	switch {
	case req.Stateless():
		return "read-only"
	case sandbox == agent.SandboxExternal:
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

// turnSandboxPolicy is the per-turn policy. externalSandbox tells Codex that an outer
// boundary (the container) enforces isolation and network policy.
func turnSandboxPolicy(req agent.Request, sandbox agent.SandboxMode) map[string]any {
	switch {
	case req.Stateless():
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case sandbox == agent.SandboxExternal:
		return map[string]any{"type": "externalSandbox", "networkAccess": "enabled"}
	default:
		roots := req.AddDirs
		if roots == nil {
			roots = []string{} // The app-server rejects null.
		}
		return map[string]any{
			"type": "workspaceWrite", "networkAccess": false, "writableRoots": roots,
		}
	}
}

func (c *conversation) startTurn(ctx context.Context, text string) error {
	params := map[string]any{
		"threadId":      c.threadID,
		"input":         []map[string]string{{"type": "text", "text": text}},
		"model":         c.request.Model,
		"effort":        c.request.Effort,
		"sandboxPolicy": turnSandboxPolicy(c.request, c.sandbox),
	}
	if c.request.OutputSchema != nil {
		params["outputSchema"] = c.request.OutputSchema
	}
	c.options.turnParams(params)
	// Reset before the request: the turn's first notifications can be read before its response.
	c.mu.Lock()
	c.text.Reset()
	c.tokens, c.thread, c.context = nil, nil, nil
	c.mu.Unlock()
	result, err := c.call(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.Turn.ID == "" {
		return errors.New("codex app-server returned no turn id")
	}
	c.mu.Lock()
	c.turnID = response.Turn.ID
	c.mu.Unlock()

	return nil
}

func (c *conversation) Send(text string) error {
	if err := agent.ValidateMessage(text); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return agent.ErrInputClosed
	}

	return c.startTurn(c.ctx, text)
}

func (c *conversation) Interrupt() error {
	c.mu.Lock()
	threadID, turnID, closed := c.threadID, c.turnID, c.closed
	c.mu.Unlock()
	if closed {
		return agent.ErrInputClosed
	}
	_, err := c.call(c.ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID})

	return err
}

func (c *conversation) Answer(answer agent.PermissionAnswer) error {
	c.mu.Lock()
	pending, ok := c.pending[answer.ID]
	delete(c.pending, answer.ID)
	c.mu.Unlock()
	if !ok {
		return agent.ErrUnknownPermission
	}
	if len(answer.Options) > 0 {
		return agent.Invalid("provider: Codex permission answers have no options")
	}
	decision := "decline"
	if answer.Allow {
		decision = "accept"
	}

	return c.respond(pending.id, map[string]string{"decision": decision}, nil)
}

func (c *conversation) Control(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	var control struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(request, &control); err != nil || control.Method == "" {
		return nil, agent.Invalid("codex control requires method and optional params")
	}
	var params any = map[string]any{}
	if len(control.Params) > 0 {
		params = control.Params
	}

	return c.call(ctx, control.Method, params)
}

func (c *conversation) Respond(id string, response json.RawMessage, errMessage string) error {
	c.mu.Lock()
	pending, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return agent.ErrUnknownControl
	}
	if errMessage != "" {
		return c.respond(pending.id, nil, &rpcError{Code: -32000, Message: errMessage})
	}
	var result any = map[string]any{}
	if len(response) > 0 {
		result = response
	}

	return c.respond(pending.id, result, nil)
}

func (c *conversation) CloseInput() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil
	}
	c.closed = true
	err := c.stdin.Close()
	c.mu.Unlock()

	return err
}

func (c *conversation) Wait() error {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// startFailed stops a conversation whose startup failed and returns the error to report. When
// the app-server ended on its own, for example on invalid configuration, its stderr explains
// why, so that message is added to err.
func (c *conversation) startFailed(err error) error {
	if errors.Is(err, errProcessEnded) || errors.Is(err, syscall.EPIPE) {
		select {
		case <-c.done:
		case <-time.After(startupExitGrace):
		}
	}
	c.abort()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("%w: %w", err, c.err)
	}

	return err
}

func (c *conversation) abort() {
	c.cancel()
	c.mu.Lock()
	_ = c.stdin.Close()
	c.closed = true
	c.mu.Unlock()
}

func (c *conversation) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.next.Add(1)
	key := strconv.FormatInt(id, 10)
	waiter := make(chan rpcResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil, agent.ErrInputClosed
	}
	c.waiters[key] = waiter
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.waiters, key)
		c.mu.Unlock()

		return nil, err
	}
	select {
	case result := <-waiter:
		return result.result, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errProcessEnded
	}
}

func (c *conversation) notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}

	return c.write(message)
}

func (c *conversation) respond(id json.RawMessage, result any, rpcErr *rpcError) error {
	message := map[string]any{"id": id}
	if rpcErr != nil {
		message["error"] = rpcErr
	} else {
		message["result"] = result
	}

	return c.write(message)
}

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
	_, err = c.stdin.Write(append(line, '\n'))

	return err
}

func (c *conversation) consume(cmd *exec.Cmd, stdout io.Reader, stderr *iox.TailBuffer) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), int(c.maxOutputBytes))
	var consumed int64
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if !json.Valid(line) {
			c.setError(errInvalidStream)
			c.cancel()
			break
		}
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &envelope)
		if envelope.Method == "turn/started" {
			consumed = 0
		}
		if !liveOnly(envelope.Method) { // Like Claude: live-only events are not retained.
			consumed += int64(len(line))
		}
		if consumed > c.maxOutputBytes {
			c.setError(errOutputLimit)
			c.cancel()
			break
		}
		if err := c.handle(line); err != nil {
			c.setError(err)
			c.cancel()
			break
		}
	}
	if err := scanner.Err(); err != nil {
		c.setError(fmt.Errorf("read codex app-server: %w", err))
	}
	waitErr := cmd.Wait()
	proctree.Reap(cmd.Process.Pid)
	c.mu.Lock()
	if c.err == nil && waitErr != nil && !c.closed {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		c.err = errors.New(redact.Message(message, maxErrorMessage))
	}
	for key, waiter := range c.waiters {
		waiter <- rpcResult{err: errProcessEnded}
		delete(c.waiters, key)
	}
	c.closed = true
	c.mu.Unlock()
	_ = os.RemoveAll(c.scratch)
	close(c.done)
}

func (c *conversation) setError(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *conversation) handle(raw json.RawMessage) error {
	var message rpcMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return errInvalidStream
	}
	if message.Method == "" {
		return c.deliverResponse(message)
	}
	if len(message.ID) > 0 && string(message.ID) != "null" {
		return c.handleServerRequest(message, raw)
	}

	return c.handleNotification(message, raw)
}

func (c *conversation) deliverResponse(message rpcMessage) error {
	key := idKey(message.ID)
	c.mu.Lock()
	waiter, ok := c.waiters[key]
	delete(c.waiters, key)
	c.mu.Unlock()
	if !ok {
		return nil
	}
	if message.Error != nil {
		waiter <- rpcResult{err: fmt.Errorf("codex RPC %d: %s", message.Error.Code, message.Error.Message)}
	} else {
		waiter <- rpcResult{result: message.Result}
	}

	return nil
}

func (c *conversation) handleServerRequest(message rpcMessage, raw json.RawMessage) error {
	id := "rpc:" + idKey(message.ID)
	pending := pendingRequest{id: append(json.RawMessage(nil), message.ID...), method: message.Method}
	c.mu.Lock()
	c.pending[id] = pending
	c.mu.Unlock()
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var params struct {
			Command string `json:"command"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(message.Params, &params)
		description := params.Command
		if description == "" {
			description = params.Reason
		}
		kind, tool := agent.PermissionCommand, "command execution"
		if strings.Contains(message.Method, "fileChange") {
			kind, tool = agent.PermissionFileChange, "file change"
		}

		if description == "" {
			description = tool
		}
		return c.onEvent(agent.Event{Type: message.Method, Raw: raw, Permission: &agent.PermissionRequest{
			ID: id, Kind: kind, Summary: description,
			Provider: agent.PermissionProvider{Provider: agent.NewProvider(Name, RequestFields{Method: message.Method}, message.Params)},
		}})
	default:
		return c.onEvent(agent.Event{Type: message.Method, Raw: raw, Control: &agent.ControlRequest{
			ID: id, Provider: agent.ControlProvider{Provider: agent.NewProvider(Name, RequestFields{Method: message.Method}, message.Params)},
		}})
	}
}

// liveOnly reports whether a notification is an incremental preview: message, reasoning and plan
// text deltas, command and process output deltas, tool progress. The item they belong to arrives
// complete in item/completed, so they are partial events, live-only, which readers receive with
// ?partial=true. Retaining them would let one command with a long output fill the run's event
// limit (about one event per output line).
func liveOnly(method string) bool {
	return strings.HasSuffix(method, "/delta") || strings.HasSuffix(method, "Delta") ||
		strings.HasSuffix(method, "/progress")
}

func (c *conversation) handleNotification(message rpcMessage, raw json.RawMessage) error {
	event := agent.Event{Type: message.Method, Raw: raw, Partial: liveOnly(message.Method)}
	switch message.Method {
	case "turn/started":
		event.TurnStarted = true
	case "item/agentMessage/delta":
		var params struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(message.Params, &params)
		c.mu.Lock()
		c.text.WriteString(params.Delta)
		c.mu.Unlock()
	case "thread/tokenUsage/updated":
		c.captureUsage(message.Params)
	case "item/started", "item/completed":
		event.Observation = observation(message.Method, message.Params)
	case "turn/completed":
		result, err := c.completed(message.Params, raw)
		if err != nil {
			return err
		}
		event.Result = result
	case "error":
		var params struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(message.Params, &params)
		if params.Message != "" {
			return errors.New(params.Message)
		}
	}

	return c.onEvent(event)
}

// nativeTokens is a Codex token breakdown. Its inputTokens include the cached and cache-write
// input (totalTokens = inputTokens + outputTokens).
type nativeTokens struct {
	Input       int64 `json:"inputTokens"`
	Output      int64 `json:"outputTokens"`
	CachedInput int64 `json:"cachedInputTokens"`
	CacheWrite  int64 `json:"cacheWriteInputTokens"`
}

// common returns the gateway breakdown, whose input excludes cached and cache-write input as
// Claude's does.
func (t nativeTokens) common() agent.Tokens {
	input := max(t.Input-t.CachedInput-t.CacheWrite, 0)
	return agent.Tokens{Input: &input, Output: &t.Output, CacheRead: &t.CachedInput, CacheCreation: &t.CacheWrite}
}

func (t *nativeTokens) add(other nativeTokens) {
	t.Input += other.Input
	t.Output += other.Output
	t.CachedInput += other.CachedInput
	t.CacheWrite += other.CacheWrite
}

// captureUsage records one model call: last is that call, total the thread so far. A turn
// with tool calls makes several model calls, so the turn's counts are the sum of every last.
func (c *conversation) captureUsage(params json.RawMessage) {
	var update struct {
		TokenUsage struct {
			Last               nativeTokens `json:"last"`
			Total              nativeTokens `json:"total"`
			ModelContextWindow *int64       `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &update) != nil {
		return
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = &nativeTokens{}
	}
	c.tokens.add(update.TokenUsage.Last)
	c.thread = &ThreadUsage{Tokens: update.TokenUsage.Total.common()}
	c.context = nil
	if window := update.TokenUsage.ModelContextWindow; window != nil {
		c.context = &agent.ContextUsage{Window: *window, Used: update.TokenUsage.Last.Input}
	}
	c.mu.Unlock()
}

func (c *conversation) completed(params, raw json.RawMessage) (*agent.Result, error) {
	var notification struct {
		Turn struct {
			Status string `json:"status"`
			Items  []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"items"`
			Error json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &notification); err != nil {
		return nil, errInvalidStream
	}
	c.mu.Lock()
	text := c.text.String()
	for _, item := range notification.Turn.Items {
		if item.Type == "agentMessage" {
			text = item.Text
		}
	}
	var tokens *agent.Tokens
	if c.tokens != nil {
		turn := c.tokens.common()
		tokens = &turn
	}
	thread, usage := c.thread, c.context
	c.mu.Unlock()
	result := outcome(notification.Turn.Status)
	failure := ""
	if result != agent.OutcomeCompleted {
		failure = turnError(notification.Turn.Error, notification.Turn.Status)
	}

	return &agent.Result{
		Text: text, Outcome: result, Error: failure, Tokens: tokens, Context: usage,
		Provider: agent.ResultProvider{Provider: agent.NewProvider(Name, ResultFields{Reason: notification.Turn.Status, Thread: thread}, raw)},
	}, nil
}

// turnError describes a turn that did not complete: the native error message, or the native
// error object when it has none, or the status.
func turnError(native json.RawMessage, status string) string {
	var described struct {
		Message string `json:"message"`
	}
	message := ""
	if len(native) > 0 && string(native) != "null" {
		if json.Unmarshal(native, &described) == nil && described.Message != "" {
			message = described.Message
		} else {
			message = string(native)
		}
	}
	if message = redact.Message(message, maxErrorMessage); message == "" {
		message = "codex turn " + status
	}

	return message
}

func observation(method string, params json.RawMessage) *agent.Observation {
	var notification struct {
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Command string `json:"command"`
			Status  string `json:"status"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.Item.ID == "" {
		return nil
	}
	kind := "task"
	switch notification.Item.Type {
	case "collabAgentToolCall", "subAgentActivity":
		kind = "subagent"
	case "commandExecution":
		kind = "process"
	}
	state := agent.ObservationActive
	if method == "item/completed" {
		switch notification.Item.Status {
		case "failed", "declined":
			state = agent.ObservationFailed
		case "interrupted":
			state = agent.ObservationCancelled
		default:
			state = agent.ObservationCompleted
		}
	}
	label := notification.Item.Command
	if label == "" {
		label = notification.Item.Type
	}

	return &agent.Observation{ID: notification.Item.ID, Kind: kind, State: state, Label: label}
}

func idKey(id json.RawMessage) string {
	var text string
	if json.Unmarshal(id, &text) == nil {
		return text
	}

	return string(id)
}
