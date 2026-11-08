package agent

import (
	"encoding/json"
	"slices"
)

// Settings are the client-chosen run settings, grouped as in the public API. A session
// remembers the settings of its last run and its next run reuses them.
type Settings struct {
	Model        ModelSettings        `json:"model"`
	Tools        *ToolSettings        `json:"tools,omitempty"`
	Instructions *InstructionSettings `json:"instructions,omitempty"`
	Output       *OutputSettings      `json:"output,omitempty"`
	// Provider is the provider section: {"name": "<agent>", the agent's own options}.
	Provider json.RawMessage `json:"provider,omitempty"`
}

// ModelSettings select the model and its reasoning effort.
type ModelSettings struct {
	ID     string `json:"id"     doc:"Model ID from GET /v1/agent/models (or a native model name)." example:"sonnet"`
	Effort string `json:"effort" doc:"Reasoning effort; one of the model's efforts from GET /v1/agent/models." example:"high"`
}

// ToolSettings decide how the agent may use tools in a workspace.
type ToolSettings struct {
	Permissions string   `json:"permissions"        enum:"bypass,ask" doc:"bypass: tools run without asking; ask: every tool use waits for an answer."`
	AddDirs     []string `json:"add_dirs,omitempty" maxItems:"16"     doc:"Extra directories the agent may use (inside an allowed root or the workspace)."`
}

// InstructionSettings shape the agent's behavior.
type InstructionSettings struct {
	SystemPrompt string `json:"system_prompt" doc:"Replaces the agent's system prompt."`
}

// OutputSettings shape the answer.
type OutputSettings struct {
	Schema map[string]any `json:"schema" doc:"JSON Schema the final answer must follow."`
}

// Apply copies the settings into req.
func (s Settings) Apply(req *Request) {
	req.Model, req.Effort, req.Provider, req.Options = s.Model.ID, s.Model.Effort, s.Provider, nil
	req.Permissions, req.AddDirs, req.SystemPrompt, req.OutputSchema = "", nil, "", nil
	if s.Tools != nil {
		req.Permissions, req.AddDirs = s.Tools.Permissions, slices.Clone(s.Tools.AddDirs)
	}
	if s.Instructions != nil {
		req.SystemPrompt = s.Instructions.SystemPrompt
	}
	if s.Output != nil {
		req.OutputSchema = s.Output.Schema
	}
}

// SettingsOf returns the settings of a validated request (add_dirs resolved).
func SettingsOf(req *Request) Settings {
	settings := Settings{Model: ModelSettings{ID: req.Model, Effort: req.Effort}, Provider: req.Provider}
	if req.Permissions != "" || len(req.AddDirs) > 0 {
		settings.Tools = &ToolSettings{Permissions: req.Permissions, AddDirs: slices.Clone(req.AddDirs)}
	}
	if req.SystemPrompt != "" {
		settings.Instructions = &InstructionSettings{SystemPrompt: req.SystemPrompt}
	}
	if req.OutputSchema != nil {
		settings.Output = &OutputSettings{Schema: req.OutputSchema}
	}

	return settings
}

// SettingsPatch changes some settings of a session. Every group and field is optional; a
// supplied field replaces the remembered one, an omitted field keeps it.
type SettingsPatch struct {
	Model        *ModelPatch       `json:"model,omitempty"`
	Tools        *ToolPatch        `json:"tools,omitempty"`
	Instructions *InstructionPatch `json:"instructions,omitempty"`
	Output       *OutputPatch      `json:"output,omitempty"`
	Provider     json.RawMessage   `json:"provider,omitempty" doc:"Replaces the remembered provider section as a whole."`
}

// ModelPatch changes the model or effort.
type ModelPatch struct {
	ID     string `json:"id,omitempty"     doc:"Model ID from GET /v1/agent/models." example:"sonnet"`
	Effort string `json:"effort,omitempty" doc:"Reasoning effort; one of the model's efforts." example:"high"`
}

// ToolPatch changes the tool settings.
type ToolPatch struct {
	Permissions string    `json:"permissions,omitempty" enum:"bypass,ask"`
	AddDirs     *[]string `json:"add_dirs,omitempty"    maxItems:"16" doc:"Replaces the list; [] removes every extra directory."`
}

// InstructionPatch changes the instructions.
type InstructionPatch struct {
	SystemPrompt *string `json:"system_prompt,omitempty" doc:"Replaces the system prompt; \"\" restores the agent's own."`
}

// OutputPatch changes the output settings.
type OutputPatch struct {
	Schema map[string]any `json:"schema,omitempty" doc:"Replaces the JSON Schema of the answer; {} removes it."`
}

// Merge returns base with the patch applied.
func (p SettingsPatch) Merge(base Settings) Settings {
	merged := base
	if p.Model != nil {
		if p.Model.ID != "" {
			merged.Model.ID = p.Model.ID
		}
		if p.Model.Effort != "" {
			merged.Model.Effort = p.Model.Effort
		}
	}
	if p.Tools != nil {
		tools := ToolSettings{}
		if base.Tools != nil {
			tools = *base.Tools
		}
		if p.Tools.Permissions != "" {
			tools.Permissions = p.Tools.Permissions
		}
		if p.Tools.AddDirs != nil {
			tools.AddDirs = slices.Clone(*p.Tools.AddDirs)
		}
		merged.Tools = &tools
	}
	if p.Instructions != nil && p.Instructions.SystemPrompt != nil {
		merged.Instructions = &InstructionSettings{SystemPrompt: *p.Instructions.SystemPrompt}
	}
	if p.Output != nil && p.Output.Schema != nil {
		merged.Output = nil
		if len(p.Output.Schema) > 0 {
			merged.Output = &OutputSettings{Schema: p.Output.Schema}
		}
	}
	if len(p.Provider) > 0 {
		merged.Provider = p.Provider
	}

	return merged
}
