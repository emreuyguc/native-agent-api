package claude

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// LoginOptions are Claude's fields of the login request's provider section.
type LoginOptions struct {
	Email string `json:"email,omitempty" doc:"Pre-fills the sign-in page (--email)."`
	SSO   bool   `json:"sso,omitempty"   doc:"Forces single sign-on (--sso)."`
}

// AllowOptions are Claude's fields of a permission allow's provider section.
type AllowOptions struct {
	UpdatedInput map[string]any `json:"updated_input,omitempty" doc:"Replaces the tool input, e.g. the answers of an AskUserQuestion request."`
}

// AuthFields are Claude's fields of the auth status provider section.
type AuthFields struct {
	Method string `json:"method,omitempty" doc:"Native authMethod, e.g. claude.ai."`
}

// ResultFields are Claude's fields of a turn result's provider section.
type ResultFields struct {
	Reason string `json:"reason" doc:"Native result subtype, e.g. success or error_max_turns."`
	// Turn is the native model_usage_since_last_result: this turn's usage by model and its
	// cost; absent when the CLI does not report it.
	Turn *Usage `json:"turn,omitempty"`
	// Session is the native total_cost_usd and modelUsage: the conversation so far. It is kept
	// across resumes and a fork starts from its parent's.
	Session Usage `json:"session"`
}

// Usage is Claude's native usage by model and its cost in USD.
type Usage struct {
	CostUSD *float64              `json:"cost_usd,omitempty"`
	Models  map[string]ModelUsage `json:"models,omitempty"`
}

// ModelUsage is one model's native usage.
type ModelUsage struct {
	Tokens          agent.Tokens `json:"tokens"`
	CostUSD         *float64     `json:"cost_usd,omitempty"`
	ContextWindow   *int64       `json:"context_window,omitempty"`
	MaxOutputTokens *int64       `json:"max_output_tokens,omitempty"`
}

// PermissionFields are Claude's fields of a permission request's provider section.
type PermissionFields struct {
	Tool string `json:"tool" doc:"Native tool name, e.g. Bash, Write or mcp__server__tool."`
}

// ControlFields are Claude's fields of a control request's provider section.
type ControlFields struct {
	Subtype string `json:"subtype" doc:"Native control request subtype, e.g. hook_callback."`
}

// ProviderSections implements agent.SectionDescriber.
func (*Agent) ProviderSections() map[agent.Section]reflect.Type { return Sections() }

// Sections describes Claude's provider sections.
func Sections() map[agent.Section]reflect.Type {
	return map[agent.Section]reflect.Type{
		agent.SectionRun:        reflect.TypeFor[RequestOptions](),
		agent.SectionLogin:      reflect.TypeFor[LoginOptions](),
		agent.SectionAllow:      reflect.TypeFor[AllowOptions](),
		agent.SectionAuth:       reflect.TypeFor[AuthFields](),
		agent.SectionResult:     reflect.TypeFor[ResultFields](),
		agent.SectionPermission: reflect.TypeFor[PermissionFields](),
		agent.SectionControl:    reflect.TypeFor[ControlFields](),
	}
}

// permissionKind maps a native tool to its gateway permission kind.
func permissionKind(tool string) string {
	switch {
	case tool == "Bash":
		return agent.PermissionCommand
	case tool == "Write" || tool == "Edit" || tool == "MultiEdit" || tool == "NotebookEdit":
		return agent.PermissionFileChange
	case strings.HasPrefix(tool, "mcp__"):
		return agent.PermissionMCP
	default:
		return agent.PermissionOther
	}
}

// permissionSummary says what the tool use does: the command, the file, or the tool.
func permissionSummary(tool, description string, input json.RawMessage) string {
	var fields struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		URL          string `json:"url"`
	}
	_ = json.Unmarshal(input, &fields)
	for _, candidate := range []string{fields.Command, fields.FilePath, fields.NotebookPath, fields.URL, description} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}

	return tool
}

// outcome maps a native result to the gateway outcome. interrupted reports whether the client
// interrupted the turn the result ends.
func outcome(subtype string, isError, interrupted bool) agent.Outcome {
	switch {
	case subtype == "success" && !isError:
		return agent.OutcomeCompleted
	case strings.HasPrefix(subtype, "error_max_"):
		return agent.OutcomeLimitReached
	case interrupted:
		return agent.OutcomeInterrupted
	default:
		return agent.OutcomeFailed
	}
}

// taskObservation maps Claude's task lifecycle events (system task_started, task_updated and
// task_notification) to an observation.
func taskObservation(subtype string, raw json.RawMessage) *agent.Observation {
	var task struct {
		TaskID       string `json:"task_id"`
		Description  string `json:"description"`
		TaskType     string `json:"task_type"`
		SubagentType string `json:"subagent_type"`
		ParentTaskID string `json:"parent_task_id"`
		Status       string `json:"status"`
		Patch        struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"patch"`
	}
	if json.Unmarshal(raw, &task) != nil || task.TaskID == "" {
		return nil
	}
	observation := &agent.Observation{ID: task.TaskID, ParentID: task.ParentTaskID}
	switch subtype {
	case "task_started":
		observation.State, observation.Label = agent.ObservationActive, task.Description
		switch {
		case task.SubagentType != "" || strings.Contains(task.TaskType, "agent"):
			observation.Kind = agent.ObservationSubagent
		case strings.Contains(task.TaskType, "bash") || strings.Contains(task.TaskType, "shell"):
			observation.Kind = agent.ObservationProcess
		default:
			observation.Kind = agent.ObservationTask
		}
	case "task_updated":
		if task.Patch.Status == "" {
			return nil // Progress without a state change.
		}
		observation.State, observation.Label = taskState(task.Patch.Status), task.Patch.Description
	case "task_notification":
		observation.State = taskState(task.Status)
	default:
		return nil
	}

	return observation
}

func taskState(status string) string {
	switch status {
	case "completed":
		return agent.ObservationCompleted
	case "failed", "error":
		return agent.ObservationFailed
	case "killed", "stopped", "cancelled":
		return agent.ObservationCancelled
	default:
		return agent.ObservationActive
	}
}
