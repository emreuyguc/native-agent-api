package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/ids"
)

// Session modes.
const (
	SessionNew    = "new"
	SessionResume = "resume"
	// SessionFork starts SessionID as a copy of the ForkFrom session.
	SessionFork = "fork"
)

// Permission modes for workspace runs.
const (
	// PermissionsBypass lets the agent use every tool without asking.
	PermissionsBypass = "bypass"
	// PermissionsAsk sends permission_request events that the client must answer.
	PermissionsAsk = "ask"
)

// Limits on request fields that apply to every agent.
const (
	MaxPromptBytes       = 1_000_000
	MaxSystemPromptBytes = 256_000
	MaxOutputSchemaBytes = 64_000
)

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// Request is one prompt sent to an agent. It is internal: the HTTP API builds it from the
// grouped public Settings and server-managed session state.
//
// Without WorkspaceID the run is stateless: no tools, no session persistence. With a
// WorkspaceID the agent runs natively in that workspace with all of its features
// (tools, skills, hooks, MCP, user/project settings) and a resumable session.
type Request struct {
	Prompt       string
	Model        string
	SystemPrompt string
	Effort       string
	OutputSchema map[string]any
	WorkspaceID  string
	SessionID    string
	SessionMode  string
	// ForkFrom is the parent session of SessionFork.
	ForkFrom string
	// Permissions is PermissionsBypass or PermissionsAsk (workspace runs only).
	Permissions string
	// Provider is the request's provider section, {"name": ..., the agent's own options}.
	Provider json.RawMessage
	// Options are the agent's own options of Provider without name, validated by the adapter
	// (see docs/agents/). Set by the server.
	Options json.RawMessage
	// AddDirs are extra directories the agent may use besides the workspace. Each must be
	// inside an allowed root or the workspace.
	AddDirs []string

	// WorkspaceDir is resolved by the server from WorkspaceID.
	WorkspaceDir string
	// RunID identifies the run; set by the server or the run manager.
	RunID string
	// Native session identifiers are opaque adapter bindings loaded by the server.
	NativeSessionID string
	NativeForkFrom  string
	// ResolveDir maps a client-supplied directory to its real path, or fails when it is
	// outside the allowed roots and the workspace. Set by the server; nil rejects all.
	ResolveDir func(dir string) (string, error)
}

// MaxAddDirs bounds add_dirs and similar directory lists.
const MaxAddDirs = 16

// Dir resolves a client-supplied directory with ResolveDir.
func (r *Request) Dir(field, dir string) (string, error) {
	if r.ResolveDir == nil {
		return "", Invalid("%s needs NATIVE_AGENT_ALLOWED_ROOTS or a directory inside the workspace", field)
	}
	resolved, err := r.ResolveDir(strings.TrimSpace(dir))
	if err != nil {
		return "", Invalid("%s: %v", field, err)
	}

	return resolved, nil
}

// Stateless reports whether the request runs without a workspace.
func (r *Request) Stateless() bool { return r.WorkspaceID == "" }

// Normalize trims fields and performs the agent-independent validation.
func (r *Request) Normalize() error {
	for _, field := range []*string{&r.Prompt, &r.Model, &r.SystemPrompt, &r.Effort, &r.WorkspaceID, &r.SessionMode, &r.Permissions} {
		*field = strings.TrimSpace(*field)
	}
	r.SessionID = strings.ToLower(strings.TrimSpace(r.SessionID))
	r.ForkFrom = strings.ToLower(strings.TrimSpace(r.ForkFrom))

	if ValidateMessage(r.Prompt) != nil {
		return Invalid("prompt must contain between 1 and %d bytes", MaxPromptBytes)
	}
	switch {
	case !modelPattern.MatchString(r.Model):
		return Invalid("model.id is required and contains unsupported characters")
	case len(r.SystemPrompt) > MaxSystemPromptBytes:
		return Invalid("instructions.system_prompt exceeds %d bytes", MaxSystemPromptBytes)
	case r.Effort == "":
		return Invalid("model.effort is required")
	}
	if r.OutputSchema != nil {
		if encoded, err := json.Marshal(r.OutputSchema); err != nil || len(encoded) > MaxOutputSchemaBytes {
			return Invalid("output.schema must be valid JSON no larger than %d bytes", MaxOutputSchemaBytes)
		}
	}

	if r.Stateless() {
		if r.SessionID != "" || r.SessionMode != "" || r.ForkFrom != "" || r.Permissions != "" || len(r.AddDirs) > 0 {
			return Invalid("tools needs a workspace_id: a call without a workspace runs without tools")
		}

		return nil
	}
	switch {
	case !ids.ValidWorkspaceID(r.WorkspaceID):
		return Invalid("workspace_id is invalid")
	case !ids.ValidSessionID(r.SessionID):
		return Invalid("session_id must be a valid UUID")
	case r.SessionMode != SessionNew && r.SessionMode != SessionResume && r.SessionMode != SessionFork:
		return Invalid("session_mode must be new, resume or fork")
	case (r.SessionMode == SessionFork) != (r.ForkFrom != ""):
		return Invalid("fork_from is required with session_mode fork and only allowed with it")
	case r.ForkFrom != "" && (!ids.ValidSessionID(r.ForkFrom) || r.ForkFrom == r.SessionID):
		return Invalid("fork_from must be a different valid session UUID")
	}
	if r.Permissions != PermissionsBypass && r.Permissions != PermissionsAsk {
		return Invalid("tools.permissions is required with a workspace and must be bypass or ask")
	}
	if len(r.AddDirs) > MaxAddDirs {
		return Invalid("tools.add_dirs allows at most %d entries", MaxAddDirs)
	}
	for i, dir := range r.AddDirs {
		resolved, err := r.Dir("tools.add_dirs", dir)
		if err != nil {
			return err
		}
		r.AddDirs[i] = resolved
	}

	return nil
}

// ValidateMessage checks a prompt or follow-up message.
func ValidateMessage(text string) error {
	if strings.TrimSpace(text) == "" || len(text) > MaxPromptBytes {
		return Invalid("message must contain between 1 and %d bytes", MaxPromptBytes)
	}

	return nil
}

// Invalid returns an error wrapping ErrInvalidRequest.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}
