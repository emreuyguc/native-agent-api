// Package permission keeps the permissions a client granted beyond a single request.
//
// A client answering a permission request may allow its kind (command, file_change, mcp,
// other) for the whole session or the whole workspace. Rules match by kind, so they work the
// same for every agent, and live only in gateway memory: the agent
// cannot edit them (they are not stored in the workspace or in agent settings), and they are
// dropped when the workspace is deleted or the gateway restarts. There is no global scope.
package permission

import (
	"slices"
	"sync"
)

// Scopes of an allow decision.
const (
	ScopeOnce      = "once"
	ScopeSession   = "session"
	ScopeWorkspace = "workspace"
)

// ValidScope reports whether scope is accepted in an allow answer.
func ValidScope(scope string) bool {
	return scope == ScopeOnce || scope == ScopeSession || scope == ScopeWorkspace
}

// Rules is a snapshot of one workspace's rules.
type Rules struct {
	// Workspace lists the permission kinds allowed in every session of the workspace.
	Workspace []string `json:"workspace"`
	// Sessions lists the permission kinds allowed per session id.
	Sessions map[string][]string `json:"sessions"`
}

type workspaceRules struct {
	kinds    map[string]struct{}
	sessions map[string]map[string]struct{}
}

// Store holds rules for all workspaces. It is safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	workspaces map[string]*workspaceRules
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{workspaces: make(map[string]*workspaceRules)} }

// Allow records a rule. ScopeOnce records nothing.
func (s *Store) Allow(workspaceID, sessionID, kind, scope string) {
	if workspaceID == "" || kind == "" || scope == ScopeOnce {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := s.workspaces[workspaceID]
	if rules == nil {
		rules = &workspaceRules{kinds: map[string]struct{}{}, sessions: map[string]map[string]struct{}{}}
		s.workspaces[workspaceID] = rules
	}
	switch scope {
	case ScopeWorkspace:
		rules.kinds[kind] = struct{}{}
	case ScopeSession:
		if rules.sessions[sessionID] == nil {
			rules.sessions[sessionID] = map[string]struct{}{}
		}
		rules.sessions[sessionID][kind] = struct{}{}
	}
}

// Allowed returns the scope of the rule that allows kind, if any.
func (s *Store) Allowed(workspaceID, sessionID, kind string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := s.workspaces[workspaceID]
	if rules == nil {
		return "", false
	}
	if _, ok := rules.kinds[kind]; ok {
		return ScopeWorkspace, true
	}
	if _, ok := rules.sessions[sessionID][kind]; ok {
		return ScopeSession, true
	}

	return "", false
}

// Rules returns the rules of a workspace.
func (s *Store) Rules(workspaceID string) Rules {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Rules{Workspace: []string{}, Sessions: map[string][]string{}}
	rules := s.workspaces[workspaceID]
	if rules == nil {
		return out
	}
	out.Workspace = sortedKeys(rules.kinds)
	for session, kinds := range rules.sessions {
		out.Sessions[session] = sortedKeys(kinds)
	}

	return out
}

// Clear removes every rule of a workspace.
func (s *Store) Clear(workspaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workspaces, workspaceID)
}

// ClearSession removes rules granted only to one session while preserving workspace rules.
func (s *Store) ClearSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rules := range s.workspaces {
		delete(rules.sessions, sessionID)
	}
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}
