package session_test

import (
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/ids"
	"github.com/emreuyguc/native-agent-api/internal/session"
)

const workspaceID = "0123456789abcdef0123456789abcdef"

func newRegistry(t *testing.T, root string, available func(string) bool) *session.Registry {
	t.Helper()
	registry, err := session.NewRegistry(root, available)
	if err != nil {
		t.Fatal(err)
	}

	return registry
}

func mustGet(t *testing.T, registry *session.Registry, sessionID string) session.Info {
	t.Helper()
	info, err := registry.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}

	return info
}

func TestPersistentSessionLifecycle(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	registry := newRegistry(t, root, func(string) bool { return true })
	sessionID := ids.NewSessionID()
	created, err := registry.Create(workspaceID, sessionID)
	if err != nil || created.State != session.StateIdle || created.NativeTranscript.Owner != "native_cli" {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	firstRun := ids.NewRunID()
	release, err := registry.Acquire(workspaceID, sessionID, firstRun, "new", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view := mustGet(t, registry, sessionID).View(); view.ActiveRun == nil || view.ActiveRun.RunID != firstRun || view.ActiveRun.Mode != "new" {
		t.Fatalf("active run = %+v", view.ActiveRun)
	}
	if _, err := registry.Acquire(workspaceID, sessionID, ids.NewRunID(), "resume", "", nil, nil); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("second Acquire error = %v, want ErrBusy", err)
	}
	if err := registry.Delete(sessionID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("active Delete error = %v, want ErrBusy", err)
	}
	if err := registry.Bind(workspaceID, sessionID, "native-thread-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := registry.Get(sessionID); got.NativeSessionID != "native-thread-1" {
		t.Fatalf("native binding = %q", got.NativeSessionID)
	}

	// A second process sees a persisted active record as orphaned, never as a live process.
	restarted := newRegistry(t, root, func(string) bool { return true })
	if got, err := restarted.Get(sessionID); err != nil || got.State != session.StateOrphaned {
		t.Fatalf("restarted Get = %+v, %v", got, err)
	}
	runID := ids.NewRunID()
	inputTokens := int64(13)
	usage := agent.NewUsageTotal()
	usage.Add(&agent.Tokens{Input: &inputTokens})
	if err := registry.Complete(workspaceID, sessionID, runID, "succeeded", usage); err != nil {
		t.Fatal(err)
	}
	release()
	if got, _ := registry.Get(sessionID); got.State != session.StateCompleted || got.LastRunID != runID {
		t.Fatalf("completed session = %+v", got)
	} else if view := got.View(); view.LastRun == nil || view.LastRun.RunID != runID || view.LastRun.Mode != "new" || view.ActiveRun != nil ||
		view.Runs.Count != 1 || view.Usage.Tokens.Input == nil || *view.Usage.Tokens.Input != 13 ||
		!slices.Equal(view.Usage.Partial, []string{"output", "cache_read", "cache_creation"}) || view.Lineage.Origin != session.OriginNew {
		t.Fatalf("completed session view = %+v", view)
	}
	resumeRelease, err := registry.Acquire(workspaceID, sessionID, ids.NewRunID(), "resume", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// While the next run is active, the last run keeps its own mode.
	if view := mustGet(t, registry, sessionID).View(); view.LastRun.Mode != "new" || view.ActiveRun.Mode != "resume" || view.Lineage.Origin != session.OriginNew {
		t.Fatalf("resumed session view = %+v", view)
	}
	if err := registry.Complete(workspaceID, sessionID, ids.NewRunID(), "succeeded", agent.NewUsageTotal()); err != nil {
		t.Fatal(err)
	}
	resumeRelease()
	forkID := ids.NewSessionID()
	forkRelease, err := registry.Acquire(workspaceID, forkID, ids.NewRunID(), "fork", sessionID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if forked, _ := registry.Get(forkID); forked.State != session.StateActive || forked.SessionMode != "fork" || forked.ForkFrom != sessionID ||
		forked.View().Lineage.Origin != session.OriginFork {
		t.Fatalf("forked session = %+v", forked)
	}
	if err := registry.Complete(workspaceID, forkID, ids.NewRunID(), "succeeded", agent.NewUsageTotal()); err != nil {
		t.Fatal(err)
	}
	forkRelease()
	if err := registry.Delete(forkID); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(sessionID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyFilteringAndAvailability(t *testing.T) {
	t.Parallel()
	registry := newRegistry(t, filepath.Join(t.TempDir(), "sessions"), func(string) bool { return false })
	sessionID := ids.NewSessionID()
	var (
		wins atomic.Int32
		wait sync.WaitGroup
	)
	for range 50 {
		wait.Go(func() {
			if _, err := registry.Acquire(workspaceID, sessionID, ids.NewRunID(), "resume", "", nil, nil); err == nil {
				wins.Add(1)
			}
		})
	}
	wait.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one lock winner, got %d", wins.Load())
	}
	listed := registry.List(workspaceID)
	if len(listed) != 1 || listed[0].State != session.StateUnavailable || len(registry.List("ffffffffffffffffffffffffffffffff")) != 0 {
		t.Fatalf("filtered unavailable sessions = %+v", listed)
	}
}
