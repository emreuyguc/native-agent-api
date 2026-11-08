// Package run executes agent conversations in the background and records their events.
//
// A Run owns one live agent process (agent.Conversation) and an append-only,
// sequence-numbered event log. Transports (SSE, WebSocket, the synchronous query endpoint)
// are readers of that log: they can attach at any time, resume after a given sequence
// number and detach without affecting the run. Control actions (messages, interrupts,
// permission answers, close, cancel) are methods on Run. Finished runs are kept for a
// retention period so clients can reconnect and read the outcome.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Status is the lifecycle state of a run.
type Status string

// Statuses. Every run starts as StatusRunning and ends in exactly one terminal status.
const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether the status is final.
func (s Status) Terminal() bool { return s != StatusRunning }

// Event types.
const (
	// EventAgent carries one native agent event in Data, unmodified.
	EventAgent = "agent"
	// EventTurn marks the end of a turn that answered a client message; Data is a TurnInfo.
	EventTurn = "turn"
	// EventPermissionRequest asks the client to allow or deny a tool; Data is a
	// Permission. Answer with Run.Answer.
	EventPermissionRequest = "permission_request"
	// EventPermissionResolved reports the answer to a permission request (from a client or
	// from a session/workspace rule); Data is a PermissionResolution.
	EventPermissionResolved = "permission_resolved"
	// EventControlRequest is a native control request from the agent that the client must
	// answer with Run.Respond; Data is a Control.
	EventControlRequest = "control_request"
	// EventControlResolved reports that a control request was answered; Data is a
	// ControlResolution.
	EventControlResolved = "control_resolved"
	// EventPartial carries incremental output (Data: native event). Partial events are not
	// stored: Seq is 0, they are only delivered to readers attached at that moment and are
	// dropped once the complete message is recorded.
	EventPartial = "partial"
	// EventDiagnostic carries bounded, redacted adapter diagnostics explicitly requested for
	// the run. Data is {"text":"..."}; it is retained but is not a native CLI event.
	EventDiagnostic = "diagnostic"
	// EventDone is the terminal event of a successful run; Data is the Snapshot.
	EventDone = "done"
	// EventError is the terminal event of a failed or cancelled run; Data is the Snapshot,
	// whose error says why.
	EventError = "error"
)

// Errors returned by Run control methods.
var (
	ErrCancelled      = errors.New("run cancelled by client")
	ErrFinished       = errors.New("run has finished")
	ErrNotInteractive = errors.New("run is not interactive")
	ErrInputClosed    = errors.New("run input is closed")
	// ErrUnknownPermission means the permission request is not pending.
	ErrUnknownPermission = agent.ErrUnknownPermission
	// ErrUnknownControl means the control request is not pending.
	ErrUnknownControl = agent.ErrUnknownControl
)

const (
	// maxPartials bounds the partial events buffered for the message in progress.
	maxPartials = 4096
)

// Event is one entry of a run's event log.
type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// ErrorInfo says why a run failed or was cancelled.
type ErrorInfo struct {
	Code    string `json:"code"    doc:"Stable code: cancelled, timeout, event_limit, agent_run_failed, agent_start_failed, native_session_missing, native_session_bind_failed."`
	Message string `json:"message"`
}

// TurnInfo is the payload of an EventTurn.
type TurnInfo struct {
	Turn   int          `json:"turn" doc:"Number of the turn in this run, from 1."`
	Result agent.Result `json:"result"`
}

// PermissionResolution is the payload of an EventPermissionResolved.
type PermissionResolution struct {
	ID       string `json:"permission_id"`
	Kind     string `json:"kind"     enum:"command,file_change,mcp,other"`
	Behavior string `json:"behavior" enum:"allow,deny"`
	Scope    string `json:"scope"    enum:"once,session,workspace"`
	// Auto is true when an existing session/workspace rule answered without asking.
	Auto bool `json:"auto"`
}

// ControlResolution is the payload of an EventControlResolved.
type ControlResolution struct {
	ID string `json:"control_id"`
}

// Observation is the latest known state of a native task, subagent, or background process.
type Observation struct {
	agent.Observation
	Times ObservationTimes `json:"times"`
}

// Rules are the session/workspace permission rules consulted for permission requests. They
// match by permission kind.
type Rules interface {
	Allowed(workspaceID, sessionID, kind string) (scope string, ok bool)
	Allow(workspaceID, sessionID, kind, scope string)
}

// Snapshot is the externally visible state of a run.
type Snapshot struct {
	ID     string `json:"run_id"`
	Status Status `json:"status" enum:"running,succeeded,failed,cancelled"`
	// Context is where the run works; empty for a stateless ask.
	Context  Context  `json:"context"`
	Mode     Mode     `json:"mode"`
	Progress Progress `json:"progress"`
	Waiting  Waiting  `json:"waiting"`
	// Observations are the native tasks, subagents and background processes of the run.
	Observations Observations `json:"observations"`
	// Result is the outcome of the latest turn that answered a client message.
	Result *agent.Result `json:"result,omitempty"`
	// Usage sums the token counts of every native turn of the run.
	Usage agent.UsageTotal `json:"usage"`
	// Error is set when the run failed or was cancelled.
	Error *ErrorInfo `json:"error,omitempty"`
	Times Times      `json:"times"`
	Trace Trace      `json:"trace"`
}

// Context is the workspace and session of a run.
type Context struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	// NativeSessionID is the agent's own identifier of the conversation (Claude: the session
	// ID, Codex: the thread ID).
	NativeSessionID string `json:"native_session_id,omitempty"`
}

// Mode is how the run was started.
type Mode struct {
	// Interactive runs accept further messages; an ask answers one prompt.
	Interactive bool `json:"interactive"`
	// Stateless is true for an ask without a workspace: no tools, nothing is kept, and the
	// context is empty.
	Stateless   bool   `json:"stateless"`
	Permissions string `json:"permissions,omitempty" enum:"bypass,ask" doc:"Absent for a stateless ask."`
}

// Progress is how far the run got.
type Progress struct {
	Turns  int   `json:"turns"  doc:"Turns that answered a client message."`
	Events int64 `json:"events" doc:"Stored events."`
	// InputClosed is true once no further messages are accepted.
	InputClosed bool `json:"input_closed"`
}

// Waiting are the requests the agent waits for the client to answer.
type Waiting struct {
	Permissions []Permission `json:"permissions"`
	Controls    []Control    `json:"controls"`
}

// Permission is a permission request the agent waits on; it is also the payload of an
// EventPermissionRequest.
type Permission struct {
	agent.PermissionRequest

	Times RequestTimes `json:"times"`
}

// Control is a native control request the agent waits on; it is also the payload of an
// EventControlRequest.
type Control struct {
	agent.ControlRequest

	Times RequestTimes `json:"times"`
}

// RequestTimes are the timestamps of a pending request. Waiting for an answer counts toward
// the turn timeout.
type RequestTimes struct {
	CreatedAt time.Time `json:"created_at"`
}

// Observations is the bounded observation list.
type Observations struct {
	Items []Observation `json:"items"`
	// Dropped counts observations not kept because of NATIVE_AGENT_MAX_RUN_OBSERVATIONS.
	Dropped int `json:"dropped"`
}

// Times are the run's timestamps.
type Times struct {
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when the native process started.
	StartedAt  *time.Time `json:"started_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ObservationTimes are the timestamps of an observation.
type ObservationTimes struct {
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Trace correlates the run with logs.
type Trace struct {
	// RequestID is the X-Request-ID of the HTTP request that started the run.
	RequestID string `json:"request_id,omitempty"`
}

// Run is one agent conversation executing in the background. It is safe for concurrent use.
type Run struct {
	id, requestID, workspaceID, sessionID, nativeSessionID, permissions string
	interactive                                                         bool
	rules                                                               Rules
	created, started, updated                                           time.Time
	cancel                                                              context.CancelCauseFunc
	done                                                                chan struct{}

	mu                  sync.Mutex
	conv                agent.Conversation
	early               []agent.Event // Events received before conv was set.
	status              Status
	events              []Event
	changed             chan struct{} // closed and replaced whenever events or status change
	finished            time.Time
	turns               int
	inputClosed         bool
	pending             []Permission
	controls            []Control
	partials            []Event
	partialBase         uint64 // absolute index of partials[0]
	observations        map[string]Observation
	observationOrder    []string
	observationsDropped int
	maxEvents           int
	maxObservations     int
	result              *agent.Result
	usage               agent.UsageTotal
	failure             *ErrorInfo
	turnTimer           *time.Timer
	idleTimer           *time.Timer
	exitTimer           *time.Timer
	exitTimeout         time.Duration // How long the agent may run on after its input closed.
}

// ID returns the run identifier.
func (r *Run) ID() string { return r.id }

// Done is closed when the run reaches a terminal status.
func (r *Run) Done() <-chan struct{} { return r.done }

// Cancel kills the agent process. It is a no-op for finished runs.
func (r *Run) Cancel(cause error) { r.cancel(cause) }

// Send queues a user message. Messages sent during a turn start the next turn.
func (r *Run) Send(text string) error {
	if err := agent.ValidateMessage(text); err != nil {
		return err
	}
	conv, err := r.control(true)
	if err != nil {
		return err
	}

	return mapConversationError(conv.Send(text))
}

// Interrupt stops the current turn; an interactive run then waits for the next message.
func (r *Run) Interrupt() error {
	conv, err := r.control(false)
	if err != nil {
		return err
	}

	return mapConversationError(conv.Interrupt())
}

// Answer resolves a pending permission request. An allow with scope session or workspace
// also records a rule that answers later requests for the same tool automatically.
func (r *Run) Answer(answer agent.PermissionAnswer, scope string) error {
	r.mu.Lock()
	if r.status.Terminal() {
		r.mu.Unlock()
		return ErrFinished
	}
	index := slices.IndexFunc(r.pending, func(p Permission) bool { return p.ID == answer.ID })
	if index < 0 {
		r.mu.Unlock()
		return ErrUnknownPermission
	}
	request := r.pending[index]
	r.pending = slices.Delete(r.pending, index, index+1)
	conv := r.conv
	resolution := PermissionResolution{ID: answer.ID, Kind: request.Kind, Behavior: "deny", Scope: "once"}
	if answer.Allow {
		resolution.Behavior, resolution.Scope = "allow", scope
		if r.rules != nil {
			r.rules.Allow(r.workspaceID, r.sessionID, request.Kind, scope)
		}
	}
	r.appendLocked(EventPermissionResolved, mustJSON(resolution))
	r.mu.Unlock()

	return mapConversationError(conv.Answer(answer))
}

// Respond answers a pending control request from the agent.
func (r *Run) Respond(id string, response json.RawMessage, errMessage string) error {
	r.mu.Lock()
	if r.status.Terminal() {
		r.mu.Unlock()
		return ErrFinished
	}
	index := slices.IndexFunc(r.controls, func(c Control) bool { return c.ID == id })
	if index < 0 {
		r.mu.Unlock()
		return ErrUnknownControl
	}
	r.controls = slices.Delete(r.controls, index, index+1)
	conv := r.conv
	r.appendLocked(EventControlResolved, mustJSON(ControlResolution{ID: id}))
	r.mu.Unlock()

	return mapConversationError(conv.Respond(id, response, errMessage))
}

// Control sends a native control request to the agent and returns its response.
func (r *Run) Control(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	conv, err := r.control(false)
	if err != nil {
		return nil, err
	}
	response, err := conv.Control(ctx, request)

	return response, mapConversationError(err)
}

// CloseInput ends the conversation gracefully after the current turn.
func (r *Run) CloseInput() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Terminal() {
		return ErrFinished
	}

	return r.closeInputLocked()
}

func (r *Run) closeInputLocked() error {
	if r.inputClosed {
		return nil
	}
	r.inputClosed = true
	r.stopTimer(&r.idleTimer)
	r.armLocked(&r.exitTimer, r.exitTimeout, func() { r.cancel(errExitTimeout) })
	r.notifyLocked()

	return r.conv.CloseInput()
}

// control returns the conversation if the run accepts control actions.
func (r *Run) control(message bool) (agent.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.status.Terminal():
		return nil, ErrFinished
	case message && !r.interactive:
		return nil, ErrNotInteractive
	case message && r.inputClosed:
		return nil, ErrInputClosed
	}

	return r.conv, nil
}

func mapConversationError(err error) error {
	if errors.Is(err, agent.ErrInputClosed) {
		return ErrInputClosed
	}

	return err
}

// Snapshot returns the current state.
func (r *Run) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.snapshotLocked()
}

func (r *Run) snapshotLocked() Snapshot {
	snapshot := Snapshot{
		ID:      r.id,
		Status:  r.status,
		Context: Context{WorkspaceID: r.workspaceID, SessionID: r.sessionID, NativeSessionID: r.nativeSessionID},
		Mode:    Mode{Interactive: r.interactive, Stateless: r.workspaceID == "", Permissions: r.permissions},
		Progress: Progress{
			Turns: r.turns, Events: int64(len(r.events)), InputClosed: r.inputClosed || r.status.Terminal(),
		},
		Waiting: Waiting{
			Permissions: append([]Permission{}, r.pending...),
			Controls:    append([]Control{}, r.controls...),
		},
		Observations: Observations{Items: []Observation{}, Dropped: r.observationsDropped},
		Result:       r.result,
		Usage:        r.usage.Clone(),
		Error:        r.failure,
		Times:        Times{CreatedAt: r.created, UpdatedAt: r.updated},
		Trace:        Trace{RequestID: r.requestID},
	}
	if !r.started.IsZero() {
		started := r.started
		snapshot.Times.StartedAt = &started
	}
	for _, id := range r.observationOrder {
		snapshot.Observations.Items = append(snapshot.Observations.Items, r.observations[id])
	}
	if !r.finished.IsZero() {
		finished := r.finished
		snapshot.Times.FinishedAt = &finished
	}

	return snapshot
}

// observeLocked records an observation update. An update may omit kind, label and parent to
// keep the known values; a new observation without a kind is a task.
func (r *Run) observeLocked(update agent.Observation) {
	if update.ID == "" || !slices.Contains(observationStates, update.State) ||
		update.Kind != "" && !slices.Contains(observationKinds, update.Kind) {
		return
	}
	now := time.Now().UTC()
	current, exists := r.observations[update.ID]
	if !exists {
		if len(r.observations) >= r.maxObservations {
			r.observationsDropped++
			return
		}
		current.Kind = agent.ObservationTask
		current.Times.CreatedAt = now
		r.observationOrder = append(r.observationOrder, update.ID)
	}
	current.ID, current.State = update.ID, update.State
	for target, value := range map[*string]string{&current.Kind: update.Kind, &current.Label: update.Label, &current.ParentID: update.ParentID} {
		if value != "" {
			*target = value
		}
	}
	current.Times.UpdatedAt = now
	current.Times.FinishedAt = nil
	if update.State != agent.ObservationActive {
		finished := now
		current.Times.FinishedAt = &finished
	}
	r.observations[update.ID] = current
}

var (
	observationKinds  = []string{agent.ObservationTask, agent.ObservationSubagent, agent.ObservationProcess}
	observationStates = []string{agent.ObservationActive, agent.ObservationCompleted, agent.ObservationFailed, agent.ObservationCancelled}
)

// Cursor is a reader position: the last stored sequence number seen and the position in
// the partial buffer. The zero value starts at the beginning.
type Cursor struct {
	Seq     int64
	partial uint64
}

// CursorAfter returns a cursor that resumes after sequence number seq.
func CursorAfter(seq int64) Cursor { return Cursor{Seq: max(seq, 0)} }

// Read returns the events after cursor (stored events first, then the partial events of the
// message in progress), advances cursor, and returns a channel closed on the next change and
// whether the run has finished.
func (r *Run) Read(cursor *Cursor) (events []Event, changed <-chan struct{}, finished bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := min(cursor.Seq, int64(len(r.events)))
	events = append(events, r.events[start:]...)
	cursor.Seq = int64(len(r.events))
	from := max(cursor.partial, r.partialBase) - r.partialBase
	events = append(events, r.partials[from:]...)
	cursor.partial = r.partialBase + uint64(len(r.partials))

	return events, r.changed, r.status.Terminal()
}

// Stream calls send for every event after cursor until the run finishes, ctx ends or send
// fails.
func (r *Run) Stream(ctx context.Context, cursor Cursor, send func(Event) error) error {
	for {
		events, changed, finished := r.Read(&cursor)
		for _, event := range events {
			if err := send(event); err != nil {
				return err
			}
		}
		if finished {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Run) appendLocked(eventType string, data json.RawMessage) {
	if eventType != EventDone && eventType != EventError && len(r.events) >= r.maxEvents-1 {
		r.cancel(errEventLimit)
		return
	}
	now := time.Now().UTC()
	r.events = append(r.events, Event{Seq: int64(len(r.events)) + 1, Type: eventType, Time: now, Data: data})
	r.updated = now
	// A stored event completes (or follows) the message the partials belonged to.
	r.partialBase += uint64(len(r.partials))
	r.partials = nil
	r.notifyLocked()
}

func (r *Run) appendPartialLocked(data json.RawMessage) {
	if len(r.partials) == maxPartials {
		r.partials = r.partials[1:]
		r.partialBase++
	}
	r.partials = append(r.partials, Event{Type: EventPartial, Time: time.Now().UTC(), Data: data})
	r.notifyLocked()
}

func (r *Run) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Run) stopTimer(timer **time.Timer) {
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err) // Only gateway-owned types are encoded here.
	}

	return data
}
