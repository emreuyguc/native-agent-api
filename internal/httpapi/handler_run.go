package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/ids"
	"github.com/emreuyguc/native-agent-api/internal/logx"
	"github.com/emreuyguc/native-agent-api/internal/permission"
	"github.com/emreuyguc/native-agent-api/internal/run"
	"github.com/emreuyguc/native-agent-api/internal/session"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// sseHeartbeat keeps idle SSE connections alive through proxies.
const sseHeartbeat = 15 * time.Second

// streamWriteTimeout bounds each SSE or WebSocket write. A reader that accepts no bytes for this
// long (a suspended client, a lost network) is disconnected instead of holding the connection and
// the run until TCP gives up; it resumes with Last-Event-ID or ?after.
var streamWriteTimeout = time.Minute // A variable for tests only.

// writeWS writes one WebSocket message within streamWriteTimeout.
func writeWS(ctx context.Context, conn *websocket.Conn, value any) error {
	ctx, cancel := context.WithTimeout(ctx, streamWriteTimeout)
	defer cancel()

	return wsjson.Write(ctx, conn, value)
}

// errClientGone cancels an ask run when its only reader disconnects.
var errClientGone = errors.New("client disconnected")

// startRunRequest is the body of POST /v1/runs/start.
type startRunRequest struct {
	WorkspaceID  string                     `json:"workspace_id" doc:"Workspace the agent works in."`
	Prompt       string                     `json:"prompt"       doc:"First user message."`
	Model        agent.ModelSettings        `json:"model"`
	Tools        agent.ToolSettings         `json:"tools"`
	Instructions *agent.InstructionSettings `json:"instructions,omitempty"`
	Output       *agent.OutputSettings      `json:"output,omitempty"`
	Provider     runProvider                `json:"provider,omitempty"`
}

func (b *startRunRequest) settings() agent.Settings {
	tools := b.Tools
	return agent.Settings{Model: b.Model, Tools: &tools, Instructions: b.Instructions, Output: b.Output, Provider: json.RawMessage(b.Provider)}
}

// askRequest is the body of POST /v1/ask and POST /v1/ask/stream.
type askRequest struct {
	WorkspaceID  string                     `json:"workspace_id,omitempty" doc:"Work natively in this workspace; the answer's session can be continued. Omit for a model call without tools in a private, throwaway directory: nothing is kept."`
	Prompt       string                     `json:"prompt"                 doc:"The question or task."`
	Model        agent.ModelSettings        `json:"model"`
	Tools        *agent.ToolSettings        `json:"tools,omitempty" doc:"Required with workspace_id; not allowed without it."`
	Instructions *agent.InstructionSettings `json:"instructions,omitempty"`
	Output       *agent.OutputSettings      `json:"output,omitempty"`
	Provider     runProvider                `json:"provider,omitempty"`
}

func (b *askRequest) request() agent.Request {
	req := agent.Request{Prompt: b.Prompt, WorkspaceID: b.WorkspaceID}
	agent.Settings{
		Model: b.Model, Tools: b.Tools, Instructions: b.Instructions, Output: b.Output, Provider: json.RawMessage(b.Provider),
	}.Apply(&req)
	if strings.TrimSpace(req.WorkspaceID) != "" {
		req.SessionID, req.SessionMode = ids.NewSessionID(), agent.SessionNew
	}

	return req
}

// continueRequest is the body of POST /v1/sessions/{session_id}/continue. Omitted settings
// reuse the session's previous ones.
type continueRequest struct {
	Prompt       string                  `json:"prompt" doc:"Next user message."`
	Model        *agent.ModelPatch       `json:"model,omitempty"`
	Tools        *agent.ToolPatch        `json:"tools,omitempty"`
	Instructions *agent.InstructionPatch `json:"instructions,omitempty"`
	Output       *agent.OutputPatch      `json:"output,omitempty"`
	Provider     runProvider             `json:"provider,omitempty" doc:"Replaces the remembered provider section as a whole."`
}

func (c *continueRequest) patch() agent.SettingsPatch {
	return agent.SettingsPatch{
		Model: c.Model, Tools: c.Tools, Instructions: c.Instructions, Output: c.Output, Provider: json.RawMessage(c.Provider),
	}
}

// messageRequest is the body of POST /v1/runs/{run_id}/message.
type messageRequest struct {
	Prompt string `json:"prompt" minLength:"1" doc:"Next user message."`
}

// permissionDecision answers a permission request (REST allow/deny or the WebSocket message).
type permissionDecision struct {
	Behavior string `json:"behavior" enum:"allow,deny"`
	Scope    string `json:"scope" enum:"once,session,workspace" doc:"Required decision scope; session/workspace are valid only for allow."`
	Message  string `json:"message,omitempty" doc:"Reason shown to the agent (deny)."`
	// Provider carries the agent's own allow options.
	Provider allowProvider `json:"provider,omitempty"`
}

func (d permissionDecision) answer(id, served string) (agent.PermissionAnswer, string, error) {
	scope := d.Scope
	switch {
	case d.Behavior != "allow" && d.Behavior != "deny":
		return agent.PermissionAnswer{}, "", agent.Invalid("behavior must be allow or deny")
	case !permission.ValidScope(scope):
		return agent.PermissionAnswer{}, "", agent.Invalid("scope is required and must be once, session or workspace")
	case d.Behavior == "deny" && scope != permission.ScopeOnce:
		return agent.PermissionAnswer{}, "", agent.Invalid("only allow decisions can have a session or workspace scope")
	}
	options, err := agent.SplitProvider(json.RawMessage(d.Provider), "provider", served)
	if err != nil {
		return agent.PermissionAnswer{}, "", err
	}

	return agent.PermissionAnswer{ID: id, Allow: d.Behavior == "allow", Message: d.Message, Options: options}, scope, nil
}

// nativeControlRequest is the body of POST /v1/runs/{run_id}/control.
type nativeControlRequest struct {
	Request jsonObject `json:"request" doc:"Native control request, e.g. {\"subtype\":\"get_context_usage\"}."`
}

// controlAnswer is the body of POST /v1/runs/{run_id}/native/controls/{control_id}/answer.
type controlAnswer struct {
	Response jsonObject `json:"response,omitempty" doc:"Success payload. Exactly one of response and error."`
	Error    string     `json:"error,omitempty"    doc:"Error message sent back to the agent."`
}

func (a controlAnswer) validate() error {
	if (len(a.Response) > 0) == (a.Error != "") {
		return agent.Invalid("exactly one of response and error is required")
	}
	if len(a.Response) > 0 && !isJSONObject(a.Response) {
		return agent.Invalid("response must be a JSON object")
	}

	return nil
}

type RunPath struct {
	RunID string `path:"run_id"`
}

type PermissionPath struct {
	RunPath

	PermissionID string `path:"permission_id"`
}

type ControlPath struct {
	RunPath

	ControlID string `path:"control_id"`
}

// runAccepted is a run snapshot with the run's Location.
type runAccepted struct {
	Location string `header:"Location"`
	Body     run.Snapshot
}

// startRun validates req, takes a concurrency slot and the session lock, and starts the run.
func (s *Server) startRun(ctx context.Context, req agent.Request, interactive bool) (*run.Run, error) {
	if req.WorkspaceID != "" {
		dir, err := s.workspaces.Dir(strings.TrimSpace(req.WorkspaceID))
		if err != nil {
			return nil, s.workspaceError(ctx, err)
		}
		req.ResolveDir = func(path string) (string, error) { return s.roots.Resolve(path, dir) }
	}
	native, err := agent.SplitProvider(req.Provider, "provider", s.agent.Name())
	req.Options = native
	if err == nil {
		err = req.Normalize()
	}
	if err == nil {
		err = s.agent.Validate(&req)
	}
	if err == nil {
		err = s.models.validateEffort(ctx, s.agent, req.Model, req.Effort)
	}
	if err != nil {
		s.log.DebugContext(ctx, "run rejected", "err", err)
		return nil, agentError(err)
	}
	if !req.Stateless() {
		switch req.SessionMode {
		case agent.SessionResume:
			info, getErr := s.sessions.Get(req.SessionID)
			if getErr != nil {
				return nil, s.sessionError(ctx, getErr)
			}
			req.NativeSessionID = info.NativeSessionID
		case agent.SessionFork:
			info, getErr := s.sessions.Get(req.ForkFrom)
			if getErr != nil {
				return nil, s.sessionError(ctx, getErr)
			}
			req.NativeForkFrom = info.NativeSessionID
		}
		if req.SessionMode != agent.SessionNew && req.NativeSessionID == "" && req.NativeForkFrom == "" {
			return nil, fail(http.StatusConflict, "native_session_unavailable", "The session has no native CLI binding.")
		}
	}

	select {
	case s.slots <- struct{}{}:
	default:
		s.log.WarnContext(ctx, "run rejected: concurrency limit reached", "limit", cap(s.slots))
		return nil, huma.ErrorWithHeaders(
			fail(http.StatusTooManyRequests, "gateway_busy", "The gateway has reached its concurrency limit."),
			http.Header{"Retry-After": {"2"}})
	}
	releaseSlot := func() { <-s.slots }

	releaseSession := func() {}
	var bindSession func(string) error
	req.RunID = ids.NewRunID()
	if !req.Stateless() {
		settings := agent.SettingsOf(&req)
		release, err := s.sessions.Acquire(req.WorkspaceID, req.SessionID, req.RunID, req.SessionMode, req.ForkFrom, &settings, func() error {
			dir, err := s.workspaces.Dir(req.WorkspaceID)
			req.WorkspaceDir = dir
			return err
		})
		if err != nil {
			releaseSlot()
			if errors.Is(err, session.ErrBusy) {
				s.log.WarnContext(ctx, "run rejected: session already running", "workspace_id", req.WorkspaceID, "session_id", req.SessionID)
				return nil, fail(http.StatusConflict, "session_busy", "Another run is already active for this session.")
			}
			return nil, s.workspaceError(ctx, err)
		}
		releaseSession = release
		bindSession = func(nativeSessionID string) error {
			return s.sessions.Bind(req.WorkspaceID, req.SessionID, nativeSessionID)
		}
	}

	// Runs are detached from the request on purpose; the manager's server context bounds them.
	started := s.runs.Start(run.Options{ //nolint:contextcheck // See above.
		Agent:           s.agent,
		Request:         req,
		Interactive:     interactive,
		TurnTimeout:     s.cfg.Limits.QueryTimeout,
		IdleTimeout:     s.cfg.Limits.RunIdle,
		MaxEvents:       s.cfg.Limits.MaxRunEvents,
		MaxObservations: s.cfg.Limits.MaxRunObservations,
		Release:         func() { releaseSession(); releaseSlot() },
		Started:         bindSession,
		Finished: func(snapshot run.Snapshot) {
			if req.Stateless() {
				return
			}
			if err := s.sessions.Complete(req.WorkspaceID, req.SessionID, snapshot.ID, string(snapshot.Status), snapshot.Usage); err != nil {
				s.log.Error("persist session completion", "workspace_id", req.WorkspaceID, "session_id", req.SessionID, "run_id", snapshot.ID, "err", err)
			}
		},
		Rules:     s.rules,
		RequestID: logx.RequestID(ctx),
	})
	s.log.InfoContext(ctx, "run created", "run_id", started.ID(), "interactive", interactive,
		"permissions", req.Permissions, "workspace_id", req.WorkspaceID, "session_id", req.SessionID)

	return started, nil
}

// createRun starts a new session and its first run. The run keeps going when the client
// disconnects and accepts further messages until it is closed or idles out.
func (s *Server) createRun(ctx context.Context, in *struct{ Body startRunRequest }) (*runAccepted, error) {
	body := &in.Body
	if strings.TrimSpace(body.WorkspaceID) == "" {
		return nil, fail(http.StatusUnprocessableEntity, "invalid_request", "workspace_id is required; use /v1/ask for a call without a workspace")
	}
	req := agent.Request{Prompt: body.Prompt, WorkspaceID: body.WorkspaceID}
	body.settings().Apply(&req)
	req.SessionID, req.SessionMode = ids.NewSessionID(), agent.SessionNew

	return s.accepted(s.startRun(ctx, req, true))
}

// continueSession starts the next run of a session with its remembered settings.
func (s *Server) continueSession(ctx context.Context, in *struct {
	SessionPath
	Body continueRequest
},
) (*runAccepted, error) {
	info, err := s.sessions.Get(in.SessionID)
	if err != nil {
		return nil, s.sessionError(ctx, err)
	}
	var stored agent.Settings
	if info.Settings != nil {
		stored = *info.Settings
	}
	req := agent.Request{Prompt: in.Body.Prompt, WorkspaceID: info.WorkspaceID, SessionID: info.SessionID}
	in.Body.patch().Merge(stored).Apply(&req)
	switch {
	case info.NativeSessionID != "":
		req.SessionMode = agent.SessionResume
	case info.ForkFrom != "" && info.GatewayRuns == 0:
		req.SessionMode, req.ForkFrom = agent.SessionFork, info.ForkFrom
	default: // The first run ended before the CLI reported a conversation: start it again.
		req.SessionMode = agent.SessionNew
	}

	return s.accepted(s.startRun(ctx, req, true))
}

// forkSession registers a copy of a session's conversation; its first continue forks natively.
func (s *Server) forkSession(ctx context.Context, in *SessionPath) (*body[session.View], error) {
	info, err := s.sessions.Fork(in.SessionID, ids.NewSessionID())
	if err != nil {
		return nil, s.sessionError(ctx, err)
	}
	s.log.InfoContext(ctx, "session forked", "session_id", info.SessionID, "fork_from", in.SessionID)

	return reply(info.View()), nil
}

func (*Server) accepted(started *run.Run, err error) (*runAccepted, error) {
	if err != nil {
		return nil, err
	}

	return &runAccepted{Location: "/v1/runs/" + started.ID(), Body: started.Snapshot()}, nil
}

// ask answers one prompt and waits for the final snapshot. The run is tied to this request:
// disconnecting cancels it.
func (s *Server) ask(ctx context.Context, in *struct{ Body askRequest }) (*huma.StreamResponse, error) {
	if in.Body.Tools != nil && in.Body.Tools.Permissions == agent.PermissionsAsk {
		return nil, fail(http.StatusUnprocessableEntity, "invalid_request",
			`tools.permissions "ask" needs an event stream to answer permission requests: use /v1/ask/stream`)
	}
	started, err := s.startRun(ctx, in.Body.request(), false)
	if err != nil {
		return nil, err
	}

	return &huma.StreamResponse{Body: func(hc huma.Context) { //nolint:contextcheck // The stream uses the request context from hc.
		r, w := humago.Unwrap(hc)
		stop := context.AfterFunc(r.Context(), func() { started.Cancel(errClientGone) })
		defer stop()
		w.Header().Set("Location", "/v1/runs/"+started.ID())
		w.Header().Set("X-Run-ID", started.ID())
		select {
		case <-started.Done():
		case <-r.Context().Done():
			return
		}
		snapshot := started.Snapshot()
		if snapshot.Error != nil {
			status := http.StatusBadGateway
			if snapshot.Error.Code == "timeout" {
				status = http.StatusGatewayTimeout
			}
			writeError(w, r, status, snapshot.Error.Code, snapshot.Error.Message)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	}}, nil
}

// askStream answers one prompt as the run's SSE stream. The run is tied to this request:
// disconnecting cancels it.
type askStreamInput struct {
	Partial bool `query:"partial" doc:"Also deliver live partial events (token deltas)."`
	Body    askRequest
}

func (s *Server) askStream(ctx context.Context, in *askStreamInput) (*huma.StreamResponse, error) {
	started, err := s.startRun(ctx, in.Body.request(), false)
	if err != nil {
		return nil, err
	}

	return &huma.StreamResponse{Body: func(hc huma.Context) { //nolint:contextcheck // The stream uses the request context from hc.
		r, w := humago.Unwrap(hc)
		stop := context.AfterFunc(r.Context(), func() { started.Cancel(errClientGone) })
		defer stop()
		w.Header().Set("Location", "/v1/runs/"+started.ID())
		s.writeSSE(w, r, started, 0, in.Partial)
	}}, nil
}

type runListInput struct {
	PageInput
	Status      string `query:"status" enum:"running,succeeded,failed,cancelled" doc:"Optional lifecycle-state filter."`
	WorkspaceID string `query:"workspace_id" doc:"Optional workspace ID filter."`
	SessionID   string `query:"session_id" doc:"Optional session ID filter."`
}

func (s *Server) listRuns(_ context.Context, input *runListInput) (*body[list[run.Snapshot]], error) {
	if input.WorkspaceID != "" && !ids.ValidWorkspaceID(input.WorkspaceID) ||
		input.SessionID != "" && !ids.ValidSessionID(input.SessionID) {
		return nil, fail(http.StatusUnprocessableEntity, "invalid_filter", "workspace_id or session_id is invalid")
	}
	all := s.runs.List()
	filtered := all[:0]
	for index := range all {
		snapshot := &all[index]
		if input.Status != "" && string(snapshot.Status) != input.Status ||
			input.WorkspaceID != "" && snapshot.Context.WorkspaceID != input.WorkspaceID ||
			input.SessionID != "" && snapshot.Context.SessionID != input.SessionID {
			continue
		}
		filtered = append(filtered, *snapshot)
	}
	return page(filtered, input.PageInput, func(item run.Snapshot) string { return item.ID })
}

func (s *Server) getRun(_ context.Context, in *RunPath) (*body[run.Snapshot], error) {
	found, err := s.lookupRun(in.RunID)
	if err != nil {
		return nil, err
	}

	return reply(found.Snapshot()), nil
}

// runAction returns a handler that applies action and answers 202 with the snapshot.
func (s *Server) runAction(name string, action func(*run.Run) error) func(context.Context, *RunPath) (*body[run.Snapshot], error) {
	return func(ctx context.Context, in *RunPath) (*body[run.Snapshot], error) {
		return s.applyAction(ctx, in.RunID, name, action)
	}
}

func (s *Server) applyAction(ctx context.Context, runID, name string, action func(*run.Run) error) (*body[run.Snapshot], error) {
	found, err := s.lookupRun(runID)
	if err != nil {
		return nil, err
	}
	if err := action(found); err != nil {
		s.log.DebugContext(ctx, "run action rejected", "run_id", found.ID(), "action", name, "err", err)
		return nil, runError(err)
	}
	s.log.InfoContext(ctx, "run action", "run_id", found.ID(), "action", name, "via", "rest")

	return reply(found.Snapshot()), nil
}

func (s *Server) sendMessage(ctx context.Context, in *struct {
	RunPath

	Body messageRequest
},
) (*body[run.Snapshot], error) {
	return s.applyAction(ctx, in.RunID, "message", func(target *run.Run) error { return target.Send(in.Body.Prompt) })
}

type allowInput struct {
	PermissionPath

	Body struct {
		Scope    string        `json:"scope" enum:"once,session,workspace" doc:"once: this request; session/workspace: also allow later requests of the same kind in the session or workspace."`
		Provider allowProvider `json:"provider,omitempty"`
	}
}

type denyInput struct {
	PermissionPath

	Body *struct {
		Message string `json:"message,omitempty" doc:"Reason shown to the agent."`
	}
}

func (s *Server) allowPermission(ctx context.Context, in *allowInput) (*body[run.Snapshot], error) {
	return s.answerPermission(ctx, in.RunPath, in.PermissionID, permissionDecision{Behavior: "allow", Scope: in.Body.Scope, Provider: in.Body.Provider})
}

func (s *Server) denyPermission(ctx context.Context, in *denyInput) (*body[run.Snapshot], error) {
	decision := permissionDecision{Behavior: "deny", Scope: permission.ScopeOnce}
	if in.Body != nil {
		decision.Message = in.Body.Message
	}

	return s.answerPermission(ctx, in.RunPath, in.PermissionID, decision)
}

func (s *Server) answerPermission(ctx context.Context, path RunPath, permissionID string, decision permissionDecision) (*body[run.Snapshot], error) {
	answer, scope, err := decision.answer(permissionID, s.agent.Name())
	if err != nil {
		return nil, agentError(err)
	}

	return s.applyAction(ctx, path.RunID, "permission_"+decision.Behavior, func(target *run.Run) error { return target.Answer(answer, scope) })
}

type nativeControlResponse struct {
	Response json.RawMessage `json:"response" doc:"The agent's native response."`
}

// sendControl forwards a native control request to the agent and returns its response.
func (s *Server) sendControl(ctx context.Context, in *struct {
	RunPath

	Body nativeControlRequest
},
) (*body[nativeControlResponse], error) {
	found, err := s.lookupRun(in.RunID)
	if err != nil {
		return nil, err
	}
	if !isJSONObject(in.Body.Request) {
		return nil, agentError(agent.Invalid("request must be a JSON object with a subtype"))
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.QueryTimeout)
	defer cancel()
	response, err := found.Control(ctx, json.RawMessage(in.Body.Request))
	if err != nil {
		s.log.DebugContext(ctx, "control request failed", "run_id", found.ID(), "err", err)
		return nil, runError(err)
	}
	s.log.InfoContext(ctx, "run action", "run_id", found.ID(), "action", "control", "via", "rest")

	return reply(nativeControlResponse{Response: response}), nil
}

// respondControl answers a control request the agent sent (control_request event).
func (s *Server) respondControl(ctx context.Context, in *struct {
	ControlPath

	Body controlAnswer
},
) (*body[run.Snapshot], error) {
	if err := in.Body.validate(); err != nil {
		return nil, agentError(err)
	}

	return s.applyAction(ctx, in.RunID, "control_response", func(target *run.Run) error {
		return target.Respond(in.ControlID, json.RawMessage(in.Body.Response), in.Body.Error)
	})
}

// runError maps run control errors to API errors.
func runError(err error) error {
	status, code, message := runErrorInfo(err)
	return fail(status, code, message)
}

func runErrorInfo(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, run.ErrFinished):
		return http.StatusConflict, "run_finished", "The run has already finished."
	case errors.Is(err, run.ErrNotInteractive):
		return http.StatusConflict, "run_not_interactive", "An ask run answers a single prompt; continue its session instead."
	case errors.Is(err, run.ErrInputClosed):
		return http.StatusConflict, "run_input_closed", "The run no longer accepts messages."
	case errors.Is(err, run.ErrUnknownPermission):
		return http.StatusNotFound, "permission_not_pending", "No pending permission request has this id."
	case errors.Is(err, run.ErrUnknownControl):
		return http.StatusNotFound, "control_not_pending", "No pending control request has this id."
	case errors.Is(err, agent.ErrControlRejected):
		return http.StatusUnprocessableEntity, "control_rejected", err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "control_timeout", "The agent did not answer the control request in time."
	case errors.Is(err, errUnsupportedControl):
		return http.StatusBadRequest, "unsupported_control", err.Error()
	default:
		return agentErrorInfo(err)
	}
}

// streamInput selects a run and the resume position.
type streamInput struct {
	RunPath

	LastEventID string `header:"Last-Event-ID" doc:"Resume after this sequence number."`
	After       string `query:"after"          doc:"Same as Last-Event-ID (takes precedence)."`
	Partial     bool   `query:"partial"        doc:"Also deliver live partial events (token deltas); they are never stored or replayed."`
}

func (in *streamInput) position() (int64, error) {
	raw := strings.TrimSpace(in.LastEventID)
	if in.After != "" {
		raw = in.After
	}
	if raw == "" {
		return 0, nil
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		return 0, fail(http.StatusBadRequest, "invalid_request", "after / Last-Event-ID must be a non-negative integer.")
	}

	return after, nil
}

func (s *Server) runEvents(_ context.Context, in *streamInput) (*huma.StreamResponse, error) {
	found, err := s.lookupRun(in.RunID)
	if err != nil {
		return nil, err
	}
	after, err := in.position()
	if err != nil {
		return nil, err
	}

	return &huma.StreamResponse{Body: func(hc huma.Context) {
		r, w := humago.Unwrap(hc)
		s.writeSSE(w, r, found, after, in.Partial)
	}}, nil
}

// writeEventSSE writes one event. Partial events carry no id, so Last-Event-ID always
// points at a stored event.
func writeEventSSE(w http.ResponseWriter, event run.Event) error {
	var err error
	if event.Seq == 0 {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data)
	} else {
		_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, event.Data)
	}

	return err
}

// writeSSE streams events with Seq > after as Server-Sent Events. The SSE id is the event
// sequence number, so browsers and clients resume automatically with Last-Event-ID.
func (s *Server) writeSSE(w http.ResponseWriter, r *http.Request, target *run.Run, after int64, partial bool) {
	controller := http.NewResponseController(w)
	// Streams may outlive the server's write timeout; turn and idle timeouts bound them, and
	// streamWriteTimeout bounds each write.
	deadline := func() { _ = controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) }
	deadline()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Run-ID", target.ID())
	w.WriteHeader(http.StatusOK)
	_ = controller.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	cursor := run.CursorAfter(after)
	for {
		events, changed, finished := target.Read(&cursor)
		deadline()
		for _, event := range events {
			if event.Type == run.EventPartial && !partial {
				continue
			}
			if err := writeEventSSE(w, event); err != nil {
				return
			}
		}
		if err := controller.Flush(); err != nil || finished {
			return
		}
		select {
		case <-changed:
		case <-heartbeat.C:
			deadline()
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// wsControl is a client → server WebSocket message.
type wsControl struct {
	Type   string `json:"type"`
	Prompt string `json:"prompt,omitempty"`
	// PermissionID selects the request a "permission" message answers.
	PermissionID string `json:"permission_id,omitempty"`
	// ControlID selects the agent control request a "control_response" message answers.
	ControlID string `json:"control_id,omitempty"`
	// Request is the native control request of type "control"; ID correlates the reply.
	Request json.RawMessage `json:"request,omitempty"`
	ID      string          `json:"id,omitempty"`
	permissionDecision
	// Response / Error answer an agent control request (type "control_response").
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// runWebSocket streams events as JSON frames ({seq,type,time,data}) and accepts control
// messages: message, interrupt, permission, close and cancel.
func (s *Server) runWebSocket(_ context.Context, in *streamInput) (*huma.StreamResponse, error) {
	found, err := s.lookupRun(in.RunID)
	if err != nil {
		return nil, err
	}
	after, err := in.position()
	if err != nil {
		return nil, err
	}

	return &huma.StreamResponse{Body: func(hc huma.Context) {
		r, w := humago.Unwrap(hc)
		s.serveWebSocket(w, r, found, after, in.Partial)
	}}, nil
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request, found *run.Run, after int64, partial bool) {
	// Deadlines set by http.Server survive the hijack; clear them for a long-lived socket.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, r, nil) // Default: browsers must be same-origin.
	if err != nil {
		s.log.WarnContext(r.Context(), "websocket upgrade failed", "err", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(s.cfg.Limits.MaxBodyBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer cancel()
		for {
			var message wsControl
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				return
			}
			if message.Type == "control" {
				// Native control requests may take a while (e.g. side questions): answer
				// asynchronously so the socket keeps reading.
				go s.wsNativeControl(ctx, conn, found, message)
				continue
			}
			if err := s.applyControl(found, message); err != nil {
				_, code, text := runErrorInfo(err)
				_ = writeWS(ctx, conn, map[string]any{
					"type": "control_error",
					"data": map[string]string{"control": message.Type, "code": code, "message": text},
				})
				continue
			}
			s.log.InfoContext(ctx, "run action", "run_id", found.ID(), "action", message.Type, "via", "websocket")
		}
	}()

	err = found.Stream(ctx, run.CursorAfter(after), func(event run.Event) error {
		if event.Type == run.EventPartial && !partial {
			return nil
		}
		return writeWS(ctx, conn, event)
	})
	if err != nil {
		return
	}
	_ = conn.Close(websocket.StatusNormalClosure, "run finished")
}

var errUnsupportedControl = errors.New("unsupported control message")

func (s *Server) applyControl(target *run.Run, message wsControl) error {
	switch message.Type {
	case "message":
		return target.Send(message.Prompt)
	case "interrupt":
		return target.Interrupt()
	case "permission":
		answer, scope, err := message.answer(message.PermissionID, s.agent.Name())
		if err != nil {
			return err
		}
		return target.Answer(answer, scope)
	case "control_response":
		answer := controlAnswer{Response: jsonObject(message.Response), Error: message.Error}
		if err := answer.validate(); err != nil {
			return err
		}
		return target.Respond(message.ControlID, json.RawMessage(answer.Response), answer.Error)
	case "close":
		return target.CloseInput()
	case "cancel":
		target.Cancel(run.ErrCancelled)
		return nil
	default:
		return fmt.Errorf("%w %q", errUnsupportedControl, message.Type)
	}
}

// wsNativeControl sends a native control request and writes {"type":"control_result"} or
// {"type":"control_error"} with the client's correlation id.
func (s *Server) wsNativeControl(ctx context.Context, conn *websocket.Conn, target *run.Run, message wsControl) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.QueryTimeout)
	defer cancel()
	var (
		response json.RawMessage
		err      error
	)
	if isJSONObject(message.Request) {
		response, err = target.Control(ctx, message.Request)
	} else {
		err = agent.Invalid("request must be a JSON object with a subtype")
	}
	if err != nil {
		_, code, text := runErrorInfo(err)
		_ = writeWS(ctx, conn, map[string]any{
			"type": "control_error", "id": message.ID,
			"data": map[string]string{"control": "control", "code": code, "message": text},
		})
		return
	}
	s.log.InfoContext(ctx, "run action", "run_id", target.ID(), "action", "control", "via", "websocket")
	_ = writeWS(ctx, conn, map[string]any{"type": "control_result", "id": message.ID, "data": map[string]json.RawMessage{"response": response}})
}

func (s *Server) lookupRun(id string) (*run.Run, error) {
	found, err := s.runs.Get(id)
	if err != nil {
		return nil, fail(http.StatusNotFound, "run_not_found", "Run was not found or has expired.")
	}

	return found, nil
}
