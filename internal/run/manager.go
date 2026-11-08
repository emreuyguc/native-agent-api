package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/ids"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// maxErrorMessage bounds agent text copied into error events.
const maxErrorMessage = 2_000

// Causes used to stop a run.
var (
	ErrNotFound     = errors.New("run not found")
	errTurnTimeout  = errors.New("turn exceeded the configured timeout")
	errEventLimit   = errors.New("run exceeded the configured event limit")
	errExitTimeout  = errors.New("agent did not exit after its input closed")
	errEventHandler = errors.New("run event handler failed")
)

// Manager starts runs and keeps them addressable by ID.
type Manager struct {
	base        context.Context //nolint:containedctx // Server-lifetime context: cancelling it cancels every run.
	retention   time.Duration
	maxRetained int
	log         *slog.Logger

	mu   sync.Mutex
	runs map[string]*Run
	wg   sync.WaitGroup
}

// NewManager returns a Manager. Cancelling base cancels all running runs.
func NewManager(base context.Context, retention time.Duration, maxRetained int, logger *slog.Logger) *Manager {
	return &Manager{base: base, retention: retention, maxRetained: maxRetained, log: logger, runs: make(map[string]*Run)}
}

// Options describe one run.
type Options struct {
	Agent   agent.Agent
	Request agent.Request
	// Interactive keeps the conversation open after a turn until CloseInput, Cancel or
	// IdleTimeout. Otherwise input is closed after the first turn.
	Interactive bool
	// TurnTimeout bounds each turn (including time spent waiting for permission answers).
	TurnTimeout time.Duration
	// IdleTimeout closes an interactive run that has had no turn for this long.
	IdleTimeout     time.Duration
	MaxEvents       int
	MaxObservations int
	// Release is called once when the run ends (frees concurrency slots and session locks).
	Release func()
	// Finished receives the immutable terminal snapshot exactly once.
	Finished func(Snapshot)
	// Started persists an opaque native session binding before the run is exposed.
	Started func(nativeSessionID string) error
	// Rules answers permission requests covered by session/workspace rules (may be nil).
	Rules Rules
	// RequestID is the X-Request-ID of the HTTP request that started the run; it is logged
	// with every record of the run and shown in its snapshot.
	RequestID string
}

// Start launches the agent process and returns the run. Process start failures produce a
// run that has already failed.
func (m *Manager) Start(opts Options) *Run {
	ctx, cancel := context.WithCancelCause(m.base)
	now := time.Now().UTC()
	if opts.Request.RunID == "" {
		opts.Request.RunID = ids.NewRunID()
	}
	r := &Run{
		id:              opts.Request.RunID,
		requestID:       opts.RequestID,
		workspaceID:     opts.Request.WorkspaceID,
		sessionID:       opts.Request.SessionID,
		permissions:     opts.Request.Permissions,
		interactive:     opts.Interactive,
		rules:           opts.Rules,
		created:         now,
		updated:         now,
		cancel:          cancel,
		done:            make(chan struct{}),
		status:          StatusRunning,
		changed:         make(chan struct{}),
		observations:    make(map[string]Observation),
		usage:           agent.NewUsageTotal(),
		maxEvents:       opts.MaxEvents,
		exitTimeout:     opts.TurnTimeout,
		maxObservations: opts.MaxObservations,
	}
	// The run is listed only once it has started or failed, so no client sees it without its
	// conversation.
	defer func() {
		m.mu.Lock()
		m.pruneLocked()
		m.runs[r.id] = r
		m.mu.Unlock()
	}()

	log := m.log.With("run_id", r.id)
	if opts.RequestID != "" {
		log = log.With("request_id", opts.RequestID)
	}
	// The lock is not held while the agent starts: a native CLI may emit events before its
	// start handshake completes, and blocking their reader would deadlock the handshake.
	// Those events wait in r.early until r.conv is set.
	conv, err := opts.Agent.Start(ctx, opts.Request, func(event agent.Event) error {
		return r.handle(event, opts, log)
	})
	r.mu.Lock()
	if err != nil {
		r.early = nil
		r.mu.Unlock()
		log.Error("run failed to start", "err", err)
		r.finish(StatusFailed, &ErrorInfo{Code: "agent_start_failed", Message: err.Error()}, opts.Finished)
		cancel(nil)
		if opts.Release != nil {
			opts.Release()
		}

		return r
	}
	r.conv = conv
	if native, ok := conv.(agent.NativeSession); ok {
		r.nativeSessionID = native.NativeSessionID()
	}
	if opts.Started != nil {
		if r.nativeSessionID == "" {
			r.mu.Unlock()
			r.finish(StatusFailed, &ErrorInfo{Code: "native_session_missing", Message: "agent did not report a native session id"}, opts.Finished)
			cancel(nil)
			m.settleRejectedConversation(conv, opts.Release)

			return r
		}
		if err := opts.Started(r.nativeSessionID); err != nil {
			r.mu.Unlock()
			r.finish(StatusFailed, &ErrorInfo{Code: "native_session_bind_failed", Message: "could not persist native session binding"}, opts.Finished)
			cancel(nil)
			m.settleRejectedConversation(conv, opts.Release)

			return r
		}
	}
	r.started = time.Now().UTC()
	r.updated = r.started
	r.armLocked(&r.turnTimer, opts.TurnTimeout, func() { r.cancel(errTurnTimeout) })
	early := r.early
	r.early = nil
	for _, event := range early {
		if err := r.handleLocked(event, opts, log); err != nil {
			log.Warn("event received during start could not be handled", "err", err)
		}
	}
	r.mu.Unlock()

	m.wg.Go(func() {
		m.wait(ctx, r, opts, log)
	})

	return r
}

func (m *Manager) settleRejectedConversation(conversation agent.Conversation, release func()) {
	m.wg.Go(func() {
		_ = conversation.Wait()
		if release != nil {
			release()
		}
	})
}

// Wait blocks until every native process started by the manager has settled.
func (m *Manager) Wait() { m.wg.Wait() }

// handle records one agent event and drives turn bookkeeping.
func (r *Run) handle(event agent.Event, opts Options, log *slog.Logger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conv == nil && !r.status.Terminal() {
		r.early = append(r.early, event) // Handled once the agent has started.
		return nil
	}

	return r.handleLocked(event, opts, log)
}

func (r *Run) handleLocked(event agent.Event, opts Options, log *slog.Logger) error {
	if r.status.Terminal() {
		return errEventHandler
	}
	switch {
	case event.Partial:
		r.appendPartialLocked(event.Raw)
		return nil
	case event.Diagnostic != "":
		r.appendLocked(EventDiagnostic, mustJSON(map[string]string{"text": event.Diagnostic}))
		return nil
	case event.Permission != nil:
		return r.permissionLocked(*event.Permission, log)
	case event.Control != nil:
		control := Control{ControlRequest: *event.Control, Times: RequestTimes{CreatedAt: time.Now().UTC()}}
		r.controls = append(r.controls, control)
		r.appendLocked(EventControlRequest, mustJSON(control))
		log.Info("control request from agent", "control_id", event.Control.ID)

		return nil
	case event.TurnStarted:
		r.stopTimer(&r.idleTimer)
		r.armLocked(&r.turnTimer, opts.TurnTimeout, func() { r.cancel(errTurnTimeout) })
	}
	r.appendLocked(EventAgent, event.Raw)
	if event.Observation != nil {
		r.observeLocked(*event.Observation)
	}

	if event.Result == nil {
		return nil
	}
	r.usage.Add(event.Result.Tokens)
	if event.Result.AgentInitiated {
		r.agentTurnLocked(event.Result, opts, log)
		return nil
	}
	r.turns++
	r.result = event.Result
	r.pending, r.controls = nil, nil
	r.stopTimer(&r.turnTimer)
	r.appendLocked(EventTurn, mustJSON(TurnInfo{Turn: r.turns, Result: *event.Result}))
	log.Info("turn finished", "turn", r.turns, "outcome", event.Result.Outcome)
	if !r.interactive {
		return r.closeInputLocked()
	}
	if !r.inputClosed {
		r.armLocked(&r.idleTimer, opts.IdleTimeout, func() {
			log.Info("closing idle interactive run", "idle_timeout", opts.IdleTimeout)
			_ = r.CloseInput()
		})
	}

	return nil
}

// agentTurnLocked settles a native turn the agent started on its own. It is already stored as
// an agent event; it neither answers a client message nor ends the run's single-prompt turn.
func (r *Run) agentTurnLocked(result *agent.Result, opts Options, log *slog.Logger) {
	r.pending, r.controls = nil, nil
	log.Info("agent-initiated turn finished", "outcome", result.Outcome)
	switch {
	case !r.interactive && !r.inputClosed:
		// The client's prompt is still unanswered: its turn timer keeps running.
	case r.interactive && !r.inputClosed:
		r.stopTimer(&r.turnTimer)
		r.armLocked(&r.idleTimer, opts.IdleTimeout, func() {
			log.Info("closing idle interactive run", "idle_timeout", opts.IdleTimeout)
			_ = r.CloseInput()
		})
	default:
		r.stopTimer(&r.turnTimer) // Input closed: the exit timer bounds the remaining work.
	}
}

// permissionLocked answers from a rule when one matches, otherwise asks the client.
func (r *Run) permissionLocked(request agent.PermissionRequest, log *slog.Logger) error {
	if r.rules != nil {
		if scope, ok := r.rules.Allowed(r.workspaceID, r.sessionID, request.Kind); ok {
			r.appendLocked(EventPermissionResolved, mustJSON(PermissionResolution{
				ID: request.ID, Kind: request.Kind, Behavior: "allow", Scope: scope, Auto: true,
			}))
			log.Info("permission allowed by rule", "permission_id", request.ID, "kind", request.Kind, "scope", scope)

			return r.conv.Answer(agent.PermissionAnswer{ID: request.ID, Allow: true})
		}
	}
	pending := Permission{PermissionRequest: request, Times: RequestTimes{CreatedAt: time.Now().UTC()}}
	r.pending = append(r.pending, pending)
	r.appendLocked(EventPermissionRequest, mustJSON(pending))
	log.Info("permission requested", "permission_id", request.ID, "kind", request.Kind)

	return nil
}

func (r *Run) armLocked(timer **time.Timer, after time.Duration, fire func()) {
	r.stopTimer(timer)
	if after > 0 {
		*timer = time.AfterFunc(after, fire)
	}
}

// wait blocks until the process exits and records the outcome.
func (m *Manager) wait(ctx context.Context, r *Run, opts Options, log *slog.Logger) {
	defer r.cancel(nil)
	if opts.Release != nil {
		defer opts.Release()
	}
	err := r.conv.Wait()

	r.mu.Lock()
	r.stopTimer(&r.turnTimer)
	r.stopTimer(&r.idleTimer)
	r.stopTimer(&r.exitTimer)
	last := r.result
	r.mu.Unlock()

	cause := context.Cause(ctx)
	if errors.Is(cause, errExitTimeout) {
		// The answer is complete; only the process lingered (Claude Code waits for its background
		// tasks before exiting). Its tree was killed; the result decides the status.
		log.Warn("agent kept running after its input closed; stopped it with its processes", "exit_timeout", opts.TurnTimeout)
		cause, err = nil, nil
	}
	switch {
	case errors.Is(cause, errTurnTimeout):
		log.Warn("run timed out", "turn_timeout", opts.TurnTimeout)
		r.finish(StatusFailed, &ErrorInfo{Code: "timeout", Message: "turn exceeded the configured timeout"}, opts.Finished)
	case errors.Is(cause, errEventLimit):
		log.Warn("run event limit reached", "max_events", opts.MaxEvents)
		r.finish(StatusFailed, &ErrorInfo{Code: "event_limit", Message: "run exceeded the configured event limit"}, opts.Finished)
	case cause != nil:
		log.Info("run cancelled", "cause", cause)
		r.finish(StatusCancelled, &ErrorInfo{Code: "cancelled", Message: cause.Error()}, opts.Finished)
	case err != nil:
		r.finish(StatusFailed, &ErrorInfo{Code: "agent_run_failed", Message: redact.Message(err.Error(), maxErrorMessage)}, opts.Finished)
	case last == nil:
		r.finish(StatusFailed, &ErrorInfo{Code: "agent_run_failed", Message: "agent exited without a result"}, opts.Finished)
	case !opts.Interactive && last.Failed():
		message := last.Error
		if message == "" {
			message = fmt.Sprintf("agent turn ended: %s", last.Outcome)
		}
		r.finish(StatusFailed, &ErrorInfo{Code: "agent_run_failed", Message: message}, opts.Finished)
	default:
		r.finish(StatusSucceeded, nil, opts.Finished)
	}
}

func (r *Run) finish(status Status, failure *ErrorInfo, finished func(Snapshot)) {
	r.mu.Lock()
	r.status, r.failure = status, failure
	r.inputClosed = true
	r.pending, r.controls = nil, nil
	r.finished = time.Now().UTC()
	terminal := EventDone
	if failure != nil {
		terminal = EventError
	}
	r.appendLocked(terminal, mustJSON(r.snapshotLocked()))
	snapshot := r.snapshotLocked()
	r.mu.Unlock()
	defer close(r.done)
	if finished != nil {
		finished(snapshot)
	}
}

// Get returns a run by ID.
func (m *Manager) Get(id string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	if r, ok := m.runs[id]; ok {
		return r, nil
	}

	return nil, ErrNotFound
}

// List returns snapshots of all known runs, newest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	m.pruneLocked()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()

	snapshots := make([]Snapshot, 0, len(runs))
	for _, r := range runs {
		snapshots = append(snapshots, r.Snapshot())
	}
	slices.SortFunc(snapshots, func(a, b Snapshot) int { return b.Times.CreatedAt.Compare(a.Times.CreatedAt) })

	return snapshots
}

// Running returns the number of runs that have not finished.
func (m *Manager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, r := range m.runs {
		select {
		case <-r.Done():
		default:
			count++
		}
	}

	return count
}

// pruneLocked drops finished runs past retention, and the oldest finished runs beyond
// the configured count limit.
func (m *Manager) pruneLocked() {
	type finishedRun struct {
		id string
		at time.Time
	}
	var finished []finishedRun
	cutoff := time.Now().Add(-m.retention)
	for id, r := range m.runs {
		r.mu.Lock()
		at := r.finished
		r.mu.Unlock()
		if at.IsZero() {
			continue
		}
		if at.Before(cutoff) {
			delete(m.runs, id)
			continue
		}
		finished = append(finished, finishedRun{id, at})
	}
	if excess := len(finished) - m.maxRetained; excess > 0 {
		slices.SortFunc(finished, func(a, b finishedRun) int { return a.at.Compare(b.at) })
		for _, old := range finished[:excess] {
			delete(m.runs, old.id)
		}
	}
}
