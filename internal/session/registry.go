// Package session persists gateway-owned session metadata and coordinates active runs.
// Native transcripts remain owned by the native CLI and are never copied or deleted here.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/ids"
)

var (
	ErrBusy       = errors.New("session is busy")
	ErrNotFound   = errors.New("session not found")
	ErrExists     = errors.New("session already exists")
	ErrInvalid    = errors.New("session metadata is invalid")
	ErrNotStarted = errors.New("session has no native conversation yet")
)

const (
	StateIdle        = "idle"
	StateActive      = "active"
	StateCompleted   = "completed"
	StateOrphaned    = "orphaned"
	StateUnavailable = "unavailable"
)

type key struct{ workspaceID, sessionID string }

type NativeTranscript struct {
	Owner string `json:"owner" enum:"native_cli"`
	State string `json:"state" enum:"unverified"`
}

// Info is the minimal gateway-owned session metadata persisted across restarts. Its JSON form
// is the metadata file format; the API shows View.
type Info struct {
	WorkspaceID      string           `json:"workspace_id"`
	SessionID        string           `json:"session_id" format:"uuid"`
	State            string           `json:"state" enum:"idle,active,completed,orphaned,unavailable"`
	SessionMode      string           `json:"session_mode,omitempty" enum:"new,resume,fork"`
	ForkFrom         string           `json:"fork_from,omitempty" format:"uuid"`
	LastRunID        string           `json:"last_run_id,omitempty" format:"uuid"`
	LastRunStatus    string           `json:"last_run_status,omitempty" enum:"succeeded,failed,cancelled"`
	LastRunMode      string           `json:"last_run_mode,omitempty" enum:"new,resume,fork"`
	ActiveRunID      string           `json:"active_run_id,omitempty" format:"uuid"`
	NativeSessionID  string           `json:"native_session_id,omitempty"`
	GatewayRuns      int              `json:"gateway_runs"`
	RunsWithoutUsage int              `json:"runs_without_usage"`
	ObservedUsage    *storedUsage     `json:"observed_usage,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
	NativeTranscript NativeTranscript `json:"native_transcript"`
	// Settings are the run settings of the last run; continuing the session reuses them.
	Settings *agent.Settings `json:"settings,omitempty"`
}

// storedUsage is the persisted form of a session's usage total.
type storedUsage struct {
	Scope               string   `json:"scope"`
	TurnsReported       int      `json:"turns_reported"`
	TurnsUnreported     int      `json:"turns_unreported"`
	Partial             []string `json:"partial,omitempty"`
	InputTokens         *int64   `json:"input_tokens,omitempty"`
	OutputTokens        *int64   `json:"output_tokens,omitempty"`
	CacheReadTokens     *int64   `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64   `json:"cache_creation_tokens,omitempty"`
}

const storedUsageScope = "gateway_observed_turns"

func (u *storedUsage) total() agent.UsageTotal {
	if u == nil {
		return agent.NewUsageTotal()
	}
	total := agent.UsageTotal{
		Turns:   agent.UsageTurns{Reported: u.TurnsReported, Unreported: u.TurnsUnreported},
		Tokens:  agent.Tokens{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheCreation: u.CacheCreationTokens},
		Partial: append([]string{}, u.Partial...),
	}.Clone()

	return total
}

func storeUsage(total agent.UsageTotal) *storedUsage {
	total = total.Clone()
	return &storedUsage{
		Scope: storedUsageScope, TurnsReported: total.Turns.Reported, TurnsUnreported: total.Turns.Unreported, Partial: total.Partial,
		InputTokens: total.Tokens.Input, OutputTokens: total.Tokens.Output,
		CacheReadTokens: total.Tokens.CacheRead, CacheCreationTokens: total.Tokens.CacheCreation,
	}
}

// View is a session as the API shows it.
type View struct {
	SessionID   string `json:"session_id" format:"uuid"`
	WorkspaceID string `json:"workspace_id"`
	// NativeSessionID is the agent's own identifier of the conversation, once it started.
	NativeSessionID string  `json:"native_session_id,omitempty"`
	State           string  `json:"state" enum:"idle,active,completed,orphaned,unavailable"`
	Lineage         Lineage `json:"lineage"`
	// ActiveRun is the run working in the session while its state is active.
	ActiveRun *Run `json:"active_run,omitempty"`
	// LastRun is the latest finished run.
	LastRun *LastRun `json:"last_run,omitempty"`
	Runs    RunCount `json:"runs"`
	// Usage sums the token counts of the session's runs.
	Usage agent.UsageTotal `json:"usage"`
	// Settings are the run settings of the last run; continuing the session reuses them.
	Settings *agent.Settings `json:"settings,omitempty"`
	Times    Times           `json:"times"`
}

// Session origins.
const (
	OriginNew  = "new"
	OriginFork = "fork"
)

// Lineage is how the session's conversation started. It does not change afterwards.
type Lineage struct {
	Origin   string `json:"origin"              enum:"new,fork" doc:"new: started by POST /v1/runs or a workspace ask; fork: copied from another session."`
	ForkFrom string `json:"fork_from,omitempty" format:"uuid"   doc:"Session this one was forked from."`
}

// Run is a run of a session and how it opened the native conversation.
type Run struct {
	RunID string `json:"run_id" format:"uuid"`
	Mode  string `json:"mode"   enum:"new,resume,fork" doc:"new: started the conversation; resume: continued it; fork: started it as a native fork."`
}

// LastRun is the latest finished run of a session.
type LastRun struct {
	Run

	Status string `json:"status" enum:"succeeded,failed,cancelled"`
}

// RunCount counts a session's finished runs.
type RunCount struct {
	Count        int `json:"count"`
	WithoutTurns int `json:"without_turns" doc:"Runs that ended before any turn finished, for example cancelled before the agent answered."`
}

// Times are a session's timestamps.
type Times struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// View returns the API form of the metadata.
func (i Info) View() View {
	origin := OriginNew
	if i.ForkFrom != "" {
		origin = OriginFork
	}
	view := View{
		SessionID: i.SessionID, WorkspaceID: i.WorkspaceID, NativeSessionID: i.NativeSessionID, State: i.State,
		Lineage:  Lineage{Origin: origin, ForkFrom: i.ForkFrom},
		Runs:     RunCount{Count: i.GatewayRuns, WithoutTurns: i.RunsWithoutUsage},
		Usage:    i.ObservedUsage.total(),
		Settings: i.Settings,
		Times:    Times{CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt},
	}
	if i.State == StateActive && i.ActiveRunID != "" {
		view.ActiveRun = &Run{RunID: i.ActiveRunID, Mode: i.SessionMode}
	}
	if i.LastRunID != "" {
		view.LastRun = &LastRun{RunID: i.LastRunID, Mode: i.LastRunMode, Status: i.LastRunStatus}
	}

	return view
}

// Registry owns session metadata and active-run locks.
type Registry struct {
	root      string
	available func(string) bool

	mu      sync.Mutex
	records map[string]Info
	active  map[key]struct{}
}

// NewRegistry loads and validates persisted metadata. Active records left by a previous process
// are atomically rewritten as orphaned because their native process cannot be proven alive.
func NewRegistry(root string, available func(workspaceID string) bool) (*Registry, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("session metadata root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("initialize session metadata: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve session metadata root: %w", err)
	}
	r := &Registry{root: resolved, available: available, records: make(map[string]Info), active: make(map[key]struct{})}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("read session metadata: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(resolved, entry.Name())) //nolint:gosec // Name came from ReadDir under the configured root.
		if err != nil {
			return nil, fmt.Errorf("read session metadata %s: %w", entry.Name(), err)
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil || !valid(info) || entry.Name() != info.SessionID+".json" {
			return nil, fmt.Errorf("invalid session metadata %s", entry.Name())
		}
		if _, duplicate := r.records[info.SessionID]; duplicate {
			return nil, fmt.Errorf("duplicate session metadata %s", info.SessionID)
		}
		if info.State == StateActive {
			info.State = StateOrphaned
			info.UpdatedAt = time.Now().UTC()
			if err := r.write(info); err != nil {
				return nil, err
			}
		}
		r.records[info.SessionID] = info
	}

	return r, nil
}

// Create registers an idle session ID issued by the gateway.
func (r *Registry) Create(workspaceID, sessionID string) (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ids.ValidWorkspaceID(workspaceID) || !ids.ValidSessionID(sessionID) {
		return Info{}, ErrInvalid
	}
	if _, exists := r.records[sessionID]; exists {
		return Info{}, ErrExists
	}
	now := time.Now().UTC()
	info := Info{
		WorkspaceID: workspaceID, SessionID: sessionID, State: StateIdle, CreatedAt: now, UpdatedAt: now,
		NativeTranscript: NativeTranscript{Owner: "native_cli", State: "unverified"},
	}
	if err := r.write(info); err != nil {
		return Info{}, err
	}
	r.records[sessionID] = info

	return info, nil
}

// Acquire marks a session active. Unknown native session IDs are registered so resume/fork can
// address transcripts created outside this gateway without claiming ownership of them.
func (r *Registry) Acquire(workspaceID, sessionID, runID, mode, forkFrom string, settings *agent.Settings, check func() error) (release func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ids.ValidWorkspaceID(workspaceID) || !ids.ValidSessionID(sessionID) || !ids.ValidRunID(runID) ||
		!slices.Contains([]string{"new", "resume", "fork"}, mode) ||
		(forkFrom != "" && !ids.ValidSessionID(forkFrom)) || (mode == "fork") != (forkFrom != "") || forkFrom == sessionID {
		return nil, ErrInvalid
	}
	if check != nil {
		if err := check(); err != nil {
			return nil, err
		}
	}
	k := key{workspaceID, sessionID}
	if _, running := r.active[k]; running {
		return nil, ErrBusy
	}
	info, exists := r.records[sessionID]
	if exists && info.WorkspaceID != workspaceID {
		return nil, ErrExists
	}
	now := time.Now().UTC()
	if !exists {
		info = Info{
			WorkspaceID: workspaceID, SessionID: sessionID, CreatedAt: now,
			NativeTranscript: NativeTranscript{Owner: "native_cli", State: "unverified"},
		}
	}
	info.State, info.SessionMode, info.ActiveRunID, info.UpdatedAt = StateActive, mode, runID, now
	if forkFrom != "" {
		info.ForkFrom = forkFrom // Kept afterwards as the session's lineage.
	}
	if settings != nil {
		info.Settings = settings
	}
	if err := r.write(info); err != nil {
		return nil, err
	}
	r.records[sessionID] = info
	r.active[k] = struct{}{}

	return sync.OnceFunc(func() {
		r.mu.Lock()
		delete(r.active, k)
		r.mu.Unlock()
	}), nil
}

// Fork registers an idle session that will fork parentID on its first run. It inherits the
// parent's workspace and settings.
func (r *Registry) Fork(parentID, sessionID string) (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.records[parentID]
	if !ok {
		return Info{}, ErrNotFound
	}
	if !ids.ValidSessionID(sessionID) || sessionID == parentID {
		return Info{}, ErrInvalid
	}
	if parent.NativeSessionID == "" {
		return Info{}, ErrNotStarted
	}
	if _, exists := r.records[sessionID]; exists {
		return Info{}, ErrExists
	}
	now := time.Now().UTC()
	info := Info{
		WorkspaceID: parent.WorkspaceID, SessionID: sessionID, State: StateIdle, ForkFrom: parentID,
		Settings: parent.Settings, CreatedAt: now, UpdatedAt: now,
		NativeTranscript: NativeTranscript{Owner: "native_cli", State: "unverified"},
	}
	if err := r.write(info); err != nil {
		return Info{}, err
	}
	r.records[sessionID] = info

	return info, nil
}

// Complete records the terminal gateway run without claiming that the native transcript exists
// or that the native CLI considers the conversation closed.
func (r *Registry) Complete(workspaceID, sessionID, runID, status string, usage agent.UsageTotal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ids.ValidRunID(runID) || !slices.Contains([]string{"succeeded", "failed", "cancelled"}, status) {
		return ErrInvalid
	}
	info, ok := r.records[sessionID]
	if !ok || info.WorkspaceID != workspaceID {
		return ErrNotFound
	}
	if _, active := r.active[key{workspaceID, sessionID}]; !active {
		return ErrInvalid
	}
	info.State, info.LastRunID, info.LastRunStatus, info.UpdatedAt = StateCompleted, runID, status, time.Now().UTC()
	info.LastRunMode, info.ActiveRunID = info.SessionMode, ""
	info.GatewayRuns++
	if usage.Turns.Reported+usage.Turns.Unreported == 0 {
		info.RunsWithoutUsage++
	} else {
		total := info.ObservedUsage.total()
		total.Merge(usage)
		info.ObservedUsage = storeUsage(total)
	}
	if err := r.write(info); err != nil {
		info.State = StateOrphaned
		r.records[sessionID] = info
		return err
	}
	r.records[sessionID] = info

	return nil
}

func (r *Registry) Get(sessionID string) (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.records[sessionID]
	if !ok {
		return Info{}, ErrNotFound
	}

	return r.withAvailability(info), nil
}

// Bind records the provider-owned session identifier while the gateway session is active.
func (r *Registry) Bind(workspaceID, sessionID, nativeSessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.records[sessionID]
	if !ok || info.WorkspaceID != workspaceID {
		return ErrNotFound
	}
	if _, active := r.active[key{workspaceID, sessionID}]; !active || nativeSessionID == "" || strings.ContainsAny(nativeSessionID, "\x00\r\n") {
		return ErrInvalid
	}
	if info.NativeSessionID != "" && info.NativeSessionID != nativeSessionID {
		return ErrInvalid
	}
	info.NativeSessionID, info.UpdatedAt = nativeSessionID, time.Now().UTC()
	if err := r.write(info); err != nil {
		return err
	}
	r.records[sessionID] = info

	return nil
}

// Health verifies that the persistent metadata root is still accessible.
func (r *Registry) Health() error {
	info, err := os.Stat(r.root)
	if err != nil {
		return fmt.Errorf("inspect session storage: %w", err)
	}
	if !info.IsDir() {
		return errors.New("session storage is not a directory")
	}

	return nil
}

// List returns sessions newest first, optionally filtered to one workspace.
func (r *Registry) List(workspaceID string) []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	infos := make([]Info, 0, len(r.records))
	for sessionID := range r.records {
		info := r.records[sessionID]
		if workspaceID == "" || info.WorkspaceID == workspaceID {
			infos = append(infos, r.withAvailability(info))
		}
	}
	slices.SortFunc(infos, func(a, b Info) int { return b.CreatedAt.Compare(a.CreatedAt) })

	return infos
}

// LastUsed returns the latest session update of every workspace that has sessions.
func (r *Registry) LastUsed() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := make(map[string]time.Time)
	for sessionID := range r.records {
		info := r.records[sessionID]
		if info.UpdatedAt.After(last[info.WorkspaceID]) {
			last[info.WorkspaceID] = info.UpdatedAt
		}
	}

	return last
}

// Delete removes gateway metadata only; the native CLI transcript is never touched.
func (r *Registry) Delete(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.records[sessionID]
	if !ok {
		return ErrNotFound
	}
	if _, active := r.active[key{info.WorkspaceID, sessionID}]; active {
		return ErrBusy
	}
	if err := os.Remove(r.filename(sessionID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("delete session metadata: %w", err)
	}
	delete(r.records, sessionID)

	return nil
}

// RemoveIfIdle runs remove only when no session of the workspace is active.
func (r *Registry) RemoveIfIdle(workspaceID string, remove func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.active {
		if k.workspaceID == workspaceID {
			return ErrBusy
		}
	}

	return remove()
}

func (r *Registry) withAvailability(info Info) Info {
	if r.available != nil && !r.available(info.WorkspaceID) {
		info.State = StateUnavailable
	}

	return info
}

func (r *Registry) write(info Info) error {
	if !valid(info) {
		return ErrInvalid
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	temporary, err := os.CreateTemp(r.root, ".session-*")
	if err != nil {
		return fmt.Errorf("create session metadata: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if err = errors.Join(err, temporary.Sync(), temporary.Close()); err != nil {
		return fmt.Errorf("write session metadata: %w", err)
	}
	if err := os.Rename(name, r.filename(info.SessionID)); err != nil {
		return fmt.Errorf("commit session metadata: %w", err)
	}

	return nil
}

func (r *Registry) filename(sessionID string) string { return filepath.Join(r.root, sessionID+".json") }

func valid(info Info) bool {
	if !ids.ValidWorkspaceID(info.WorkspaceID) || !ids.ValidSessionID(info.SessionID) || info.CreatedAt.IsZero() || info.UpdatedAt.IsZero() ||
		info.NativeTranscript.Owner != "native_cli" || info.NativeTranscript.State != "unverified" {
		return false
	}
	if !slices.Contains([]string{StateIdle, StateActive, StateCompleted, StateOrphaned}, info.State) {
		return false
	}
	if info.LastRunID != "" && !ids.ValidRunID(info.LastRunID) || info.ActiveRunID != "" && !ids.ValidRunID(info.ActiveRunID) {
		return false
	}
	if info.LastRunMode != "" && !slices.Contains([]string{"new", "resume", "fork"}, info.LastRunMode) {
		return false
	}
	if info.ForkFrom != "" && !ids.ValidSessionID(info.ForkFrom) {
		return false
	}
	if info.SessionMode != "" && !slices.Contains([]string{"new", "resume", "fork"}, info.SessionMode) {
		return false
	}
	if info.LastRunStatus != "" && !slices.Contains([]string{"succeeded", "failed", "cancelled"}, info.LastRunStatus) {
		return false
	}
	if strings.ContainsAny(info.NativeSessionID, "\x00\r\n") {
		return false
	}
	if info.GatewayRuns < 0 || info.RunsWithoutUsage < 0 || info.RunsWithoutUsage > info.GatewayRuns {
		return false
	}
	if usage := info.ObservedUsage; usage != nil {
		if usage.Scope != storedUsageScope || usage.TurnsReported < 0 || usage.TurnsUnreported < 0 ||
			!validTokenUsage(usage.total().Tokens) || slices.ContainsFunc(usage.Partial, func(field string) bool {
			return !slices.Contains(agent.TokenFields[:], field)
		}) {
			return false
		}
	}
	if (info.SessionMode == "fork" && info.ForkFrom == "") || info.ForkFrom == info.SessionID {
		return false
	}
	switch info.State {
	case StateIdle:
		if info.SessionMode != "" || info.LastRunID != "" || info.LastRunStatus != "" || info.ActiveRunID != "" {
			return false
		}
	case StateActive, StateOrphaned:
		if info.SessionMode == "" {
			return false
		}
	case StateCompleted:
		if info.SessionMode == "" || info.LastRunID == "" || info.LastRunStatus == "" {
			return false
		}
	}

	return !strings.ContainsAny(info.SessionMode, "\x00\r\n") && !strings.ContainsAny(info.LastRunStatus, "\x00\r\n")
}

func validTokenUsage(tokens agent.Tokens) bool {
	for _, value := range tokens.Fields() {
		if *value != nil && **value < 0 {
			return false
		}
	}

	return true
}
