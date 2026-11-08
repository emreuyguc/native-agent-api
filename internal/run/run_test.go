package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/agenttest"
	"github.com/emreuyguc/native-agent-api/internal/logx"
	"github.com/emreuyguc/native-agent-api/internal/permission"
	"github.com/emreuyguc/native-agent-api/internal/run"
)

func newManager(t *testing.T) *run.Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return run.NewManager(ctx, time.Minute, 20, logx.Discard())
}

func options(script agenttest.Script) run.Options {
	return run.Options{
		Agent:       &agenttest.Agent{Script: script},
		Request:     agent.Request{Prompt: "hi"},
		TurnTimeout: 5 * time.Second,
		IdleTimeout: time.Minute, MaxEvents: 1_000, MaxObservations: 20,
	}
}

func wait(t *testing.T, r *run.Run) run.Snapshot {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not finish: %+v", r.Snapshot())
	}

	return r.Snapshot()
}

// waitFor polls the snapshot until cond holds.
func waitFor(t *testing.T, r *run.Run, cond func(run.Snapshot) bool) run.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := r.Snapshot()
		if cond(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached: %+v", snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func types(events []run.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}

	return out
}

func collect(t *testing.T, r *run.Run, after int64) []run.Event {
	t.Helper()
	var events []run.Event
	if err := r.Stream(t.Context(), run.CursorAfter(after), func(e run.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return events
}

func TestSingleTurnRunClosesAfterResult(t *testing.T) {
	t.Parallel()
	released := make(chan struct{})
	script := func(ctx context.Context, session *agenttest.Session) error {
		input, ok := session.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		if err := session.StartTurn(); err != nil {
			return err
		}
		if err := session.Emit("assistant", `{"type":"assistant"}`); err != nil {
			return err
		}
		if err := session.Observe(`{"type":"native","task":"one"}`, agent.Observation{
			ID: "one", Kind: "subagent", State: "completed", Label: "research",
		}); err != nil {
			return err
		}
		inputTokens, outputTokens, cacheRead, cacheCreate := int64(3), int64(5), int64(7), int64(11)
		if err := session.FinishWithUsage("echo: "+input.Text, false, &agent.Tokens{
			Input: &inputTokens, Output: &outputTokens, CacheRead: &cacheRead, CacheCreation: &cacheCreate,
		}); err != nil {
			return err
		}
		_, _ = session.Next(ctx)

		return nil
	}
	opts := options(script)
	opts.Release = func() { close(released) }
	r := newManager(t).Start(opts)

	snapshot := wait(t, r)
	if snapshot.Status != run.StatusSucceeded || snapshot.Progress.Turns != 1 || snapshot.Result.Text != "echo: hi" || !snapshot.Progress.InputClosed {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(snapshot.Usage.Partial) != 0 || snapshot.Usage.Turns.Reported != 1 || snapshot.Usage.Tokens.Input == nil || *snapshot.Usage.Tokens.Input != 3 {
		t.Fatalf("usage aggregate = %+v", snapshot.Usage)
	}
	if snapshot.Times.StartedAt == nil || snapshot.Times.UpdatedAt.Before(snapshot.Times.CreatedAt) || len(snapshot.Observations.Items) != 1 ||
		snapshot.Observations.Items[0].ID != "one" || snapshot.Observations.Items[0].Times.FinishedAt == nil {
		t.Fatalf("lifecycle snapshot = %+v", snapshot)
	}
	<-released
	events := collect(t, r, 0)
	if got := strings.Join(types(events), ","); got != "agent,agent,agent,agent,turn,done" {
		t.Fatalf("events = %s", got)
	}
	if resumed := collect(t, r, 4); len(resumed) != 2 || resumed[0].Seq != 5 {
		t.Fatalf("resume after 4 = %+v", resumed)
	}
	if err := r.Send("more"); !errors.Is(err, run.ErrFinished) {
		t.Fatalf("Send after finish = %v", err)
	}
}

func TestAgentInitiatedTurnsDoNotAnswerThePrompt(t *testing.T) {
	t.Parallel()
	// A resumed Claude session first reports a stale background-task notification, answers the
	// prompt, then reacts to another notification after its input closed.
	script := func(ctx context.Context, session *agenttest.Session) error {
		input, ok := session.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		for _, step := range []func() error{
			session.StartTurn,
			func() error { return session.AgentTurn("") },
			session.StartTurn,
			func() error { return session.Finish("echo: "+input.Text, false) },
			session.StartTurn,
			func() error { return session.AgentTurn("task done") },
		} {
			if err := step(); err != nil {
				return err
			}
		}
		_, _ = session.Next(ctx)

		return nil
	}
	r := newManager(t).Start(options(script))

	snapshot := wait(t, r)
	if snapshot.Status != run.StatusSucceeded || snapshot.Progress.Turns != 1 || snapshot.Result.Text != "echo: hi" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if got := strings.Join(types(collect(t, r, 0)), ","); got != "agent,agent,agent,agent,turn,agent,agent,done" {
		t.Fatalf("events = %s", got)
	}
}

func TestInteractiveRunAcceptsMessagesUntilClosed(t *testing.T) {
	t.Parallel()
	opts := options(agenttest.Echo)
	opts.Interactive = true
	r := newManager(t).Start(opts)

	waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	if err := r.Send("second"); err != nil {
		t.Fatal(err)
	}
	snapshot := waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 2 })
	if snapshot.Status != run.StatusRunning || snapshot.Result.Text != "echo: second" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := r.Send(" "); !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("empty message error = %v", err)
	}
	if err := r.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := r.Send("late"); !errors.Is(err, run.ErrInputClosed) {
		t.Fatalf("Send after close = %v", err)
	}
	if snapshot = wait(t, r); snapshot.Status != run.StatusSucceeded || snapshot.Progress.Turns != 2 {
		t.Fatalf("final = %+v", snapshot)
	}
}

func TestNonInteractiveRejectsMessages(t *testing.T) {
	t.Parallel()
	r := newManager(t).Start(options(agenttest.Blocking(nil)))
	if err := r.Send("more"); !errors.Is(err, run.ErrNotInteractive) {
		t.Fatalf("Send = %v", err)
	}
	r.Cancel(run.ErrCancelled)
	wait(t, r)
}

func TestIdleInteractiveRunCloses(t *testing.T) {
	t.Parallel()
	opts := options(agenttest.Echo)
	opts.Interactive = true
	opts.IdleTimeout = 30 * time.Millisecond
	if snapshot := wait(t, newManager(t).Start(opts)); snapshot.Status != run.StatusSucceeded || snapshot.Progress.Turns != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestInterruptKeepsInteractiveRunOpen(t *testing.T) {
	t.Parallel()
	opts := options(agenttest.Blocking(nil))
	opts.Interactive = true
	r := newManager(t).Start(opts)

	waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Events >= 1 })
	if err := r.Interrupt(); err != nil {
		t.Fatal(err)
	}
	snapshot := waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	if snapshot.Status != run.StatusRunning || !snapshot.Result.Failed() {
		t.Fatalf("after interrupt = %+v", snapshot)
	}
	_ = r.CloseInput()
	// Interactive runs succeed even when the last turn was interrupted.
	if final := wait(t, r); final.Status != run.StatusSucceeded {
		t.Fatalf("final = %+v", final)
	}
}

func TestPermissionFlow(t *testing.T) {
	t.Parallel()
	r := newManager(t).Start(options(agenttest.Permission))

	snapshot := waitFor(t, r, func(s run.Snapshot) bool { return len(s.Waiting.Permissions) == 1 })
	if pending := snapshot.Waiting.Permissions[0]; pending.ID != "perm-1" || pending.Kind != agent.PermissionFileChange {
		t.Fatalf("pending = %+v", pending)
	}
	if err := r.Answer(agent.PermissionAnswer{ID: "nope"}, "once"); !errors.Is(err, run.ErrUnknownPermission) {
		t.Fatalf("unknown answer = %v", err)
	}
	if err := r.Answer(agent.PermissionAnswer{ID: "perm-1", Message: "no thanks"}, "once"); err != nil {
		t.Fatal(err)
	}
	// The run may already have finished after the first answer; either way the second is refused.
	if err := r.Answer(agent.PermissionAnswer{ID: "perm-1"}, "once"); !errors.Is(err, run.ErrUnknownPermission) &&
		!errors.Is(err, run.ErrFinished) {
		t.Fatalf("double answer = %v", err)
	}
	final := wait(t, r)
	if final.Result.Text != "allow=false message=no thanks" || len(final.Waiting.Permissions) != 0 {
		t.Fatalf("final = %+v", final)
	}
	got := strings.Join(types(collect(t, r, 0)), ",")
	if !strings.Contains(got, "permission_request,permission_resolved") {
		t.Fatalf("events = %s", got)
	}
}

func TestTurnTimeoutAndFailures(t *testing.T) {
	t.Parallel()
	m := newManager(t)

	opts := options(agenttest.Blocking(nil))
	opts.TurnTimeout = 30 * time.Millisecond
	if s := wait(t, m.Start(opts)); s.Status != run.StatusFailed || s.Error.Code != "timeout" {
		t.Fatalf("timeout = %+v", s)
	}

	failing := options(func(_ context.Context, s *agenttest.Session) error {
		_ = s.Finish("boom", true)
		return nil
	})
	if s := wait(t, m.Start(failing)); s.Status != run.StatusFailed || s.Error.Message != "boom" {
		t.Fatalf("error result = %+v", s)
	}

	crashing := options(func(context.Context, *agenttest.Session) error { return errors.New("crash") })
	if s := wait(t, m.Start(crashing)); s.Status != run.StatusFailed || s.Error.Message != "crash" {
		t.Fatalf("crash = %+v", s)
	}

	eventLimited := options(func(ctx context.Context, session *agenttest.Session) error {
		for range 10 {
			if err := session.Emit("native", `{"type":"native"}`); err != nil {
				return err
			}
		}
		_, _ = session.Next(ctx)

		return nil
	})
	eventLimited.MaxEvents = 4
	if s := wait(t, m.Start(eventLimited)); s.Status != run.StatusFailed || s.Error.Code != "event_limit" || s.Progress.Events > 4 {
		t.Fatalf("event limit = %+v", s)
	}

	released := make(chan struct{})
	startFails := options(nil)
	startFails.Agent = &agenttest.Agent{StartErr: errors.New("no binary")}
	startFails.Release = func() { close(released) }
	if s := wait(t, m.Start(startFails)); s.Status != run.StatusFailed || s.Error.Code != "agent_start_failed" {
		t.Fatalf("start failure = %+v", s)
	}
	<-released
}

func TestManagerLookupAndShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	m := run.NewManager(ctx, time.Minute, 20, logx.Discard())
	r := m.Start(options(agenttest.Blocking(nil)))

	if got, err := m.Get(r.ID()); err != nil || got != r {
		t.Fatalf("Get() = %v, %v", got, err)
	}
	if _, err := m.Get("missing"); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v", err)
	}
	if m.Running() != 1 || len(m.List()) != 1 {
		t.Fatalf("Running=%d List=%d", m.Running(), len(m.List()))
	}
	cancel() // Server shutdown cancels every run.
	if snapshot := wait(t, r); snapshot.Status != run.StatusCancelled {
		t.Fatalf("snapshot after shutdown = %+v", snapshot)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	t.Parallel()
	opts := options(agenttest.Echo)
	opts.Interactive = true
	r := newManager(t).Start(opts)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if events := collect(t, r, 0); events[len(events)-1].Type != run.EventDone {
				t.Errorf("reader did not see done")
			}
		})
	}
	for range 5 {
		wg.Go(func() { _ = r.Send("x") })
	}
	waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 6 })
	_ = r.CloseInput()
	wg.Wait()

	var done run.Snapshot
	events := collect(t, r, 0)
	if err := json.Unmarshal(events[len(events)-1].Data, &done); err != nil || done.Progress.Turns != 6 {
		t.Fatalf("done payload = %+v, %v", done, err)
	}
}

func TestRulesAnswerLaterRequests(t *testing.T) {
	t.Parallel()
	rules := permission.NewStore()
	opts := options(agenttest.Permission)
	opts.Interactive = true
	opts.Rules = rules
	opts.Request.WorkspaceID, opts.Request.SessionID = "w1", "s1"
	r := newManager(t).Start(opts)

	waitFor(t, r, func(s run.Snapshot) bool { return len(s.Waiting.Permissions) == 1 })
	if err := r.Answer(agent.PermissionAnswer{ID: "perm-1", Allow: true}, permission.ScopeSession); err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	// The second request of the same kind is answered by the session rule.
	if err := r.Send("again"); err != nil {
		t.Fatal(err)
	}
	snapshot := waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 2 })
	if len(snapshot.Waiting.Permissions) != 0 || snapshot.Result.Text != "allow=true message=" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	_ = r.CloseInput()
	wait(t, r)

	var resolutions []run.PermissionResolution
	for _, event := range collect(t, r, 0) {
		if event.Type == run.EventPermissionResolved {
			var resolution run.PermissionResolution
			_ = json.Unmarshal(event.Data, &resolution)
			resolutions = append(resolutions, resolution)
		}
	}
	if len(resolutions) != 2 || resolutions[0].Auto || !resolutions[1].Auto || resolutions[1].Scope != permission.ScopeSession {
		t.Fatalf("resolutions = %+v", resolutions)
	}
	if scope, ok := rules.Allowed("w1", "s1", agent.PermissionFileChange); !ok || scope != permission.ScopeSession {
		t.Fatalf("rule not stored: %q %v", scope, ok)
	}
}

func TestControlRequestsInBothDirections(t *testing.T) {
	t.Parallel()
	r := newManager(t).Start(options(agenttest.Hook))

	snapshot := waitFor(t, r, func(s run.Snapshot) bool { return len(s.Waiting.Controls) == 1 })
	if pending := snapshot.Waiting.Controls[0]; pending.ID != "hook-1" || pending.Provider.Name != "fake" {
		t.Fatalf("pending = %+v", pending)
	}
	response, err := r.Control(t.Context(), json.RawMessage(`{"subtype":"get_context_usage"}`))
	if err != nil || !strings.Contains(string(response), "get_context_usage") {
		t.Fatalf("Control = %s, %v", response, err)
	}
	if _, err := r.Control(t.Context(), json.RawMessage(`{"subtype":"fail"}`)); !errors.Is(err, agent.ErrControlRejected) {
		t.Fatalf("rejected control error = %v", err)
	}
	if err := r.Respond("nope", nil, ""); !errors.Is(err, run.ErrUnknownControl) {
		t.Fatalf("unknown respond = %v", err)
	}
	if err := r.Respond("hook-1", json.RawMessage(`{"continue":true}`), ""); err != nil {
		t.Fatal(err)
	}
	final := wait(t, r)
	if final.Result.Text != `id=hook-1 response={"continue":true} error=` || len(final.Waiting.Controls) != 0 {
		t.Fatalf("final = %+v", final)
	}
	if got := strings.Join(types(collect(t, r, 0)), ","); !strings.Contains(got, "control_request,control_resolved") {
		t.Fatalf("events = %s", got)
	}
}

func TestPartialEventsAreLiveOnly(t *testing.T) {
	t.Parallel()
	opts := options(agenttest.Streaming)
	opts.Interactive = true
	r := newManager(t).Start(opts)

	// A live reader sees partials before the complete message.
	var live []string
	done := make(chan error, 1)
	go func() {
		done <- r.Stream(t.Context(), run.Cursor{}, func(e run.Event) error {
			live = append(live, e.Type)
			return nil
		})
	}()
	waitFor(t, r, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	_ = r.CloseInput()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored := collect(t, r, 0)
	for _, event := range stored {
		if event.Type == run.EventPartial || event.Seq == 0 {
			t.Fatalf("partial event was stored: %+v", event)
		}
	}
	// Stored: init, assistant, result (agent), turn, done. Partials, when a reader catches
	// them, sit between init and the complete assistant message, never after it.
	if got := strings.Join(types(stored), ","); got != "agent,agent,agent,turn,done" {
		t.Fatalf("stored = %s", got)
	}
	if slices.Contains(live, run.EventPartial) {
		last := len(live) - 1 - slices.Index(reversed(live), run.EventPartial)
		after := live[last+1:]
		if strings.Join(after, ",") != "agent,agent,turn,done" || live[0] != run.EventAgent {
			t.Fatalf("live order = %v", live)
		}
	}
}

func reversed(in []string) []string {
	out := slices.Clone(in)
	slices.Reverse(out)

	return out
}

// TestLingeringAgentIsStoppedAfterItsAnswer: Claude Code keeps running after its result while a
// background task runs. The run must still end (and free its slot) within the exit timeout,
// with the answer's status.
func TestLingeringAgentIsStoppedAfterItsAnswer(t *testing.T) {
	t.Parallel()
	released := make(chan struct{})
	script := func(ctx context.Context, session *agenttest.Session) error {
		if _, ok := session.Next(ctx); !ok {
			return ctx.Err()
		}
		if err := session.StartTurn(); err != nil {
			return err
		}
		if err := session.Finish("done", false); err != nil {
			return err
		}
		<-ctx.Done() // Ignores the closed input, like a CLI waiting for a background task.
		return ctx.Err()
	}
	opts := options(script)
	opts.TurnTimeout = 200 * time.Millisecond
	opts.Release = func() { close(released) }
	if s := wait(t, newManager(t).Start(opts)); s.Status != run.StatusSucceeded || s.Result == nil || s.Result.Text != "done" {
		t.Fatalf("snapshot = %+v", s)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("slot not released")
	}
}
