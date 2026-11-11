package agent

import (
	"maps"
	"slices"
)

// Capability is one native CLI surface implemented by an adapter. Gateway-wide transports and
// storage features are not listed here. Unknown future values must be ignored by API clients.
type Capability string

// Adapter capabilities. Keep values provider-neutral and add one only when clients can use it.
const (
	CapabilityAuth              Capability = "auth"
	CapabilityCustomAgents      Capability = "custom_agents"
	CapabilityDebugOutput       Capability = "debug_output"
	CapabilityDiagnostics       Capability = "diagnostics"
	CapabilityFileCheckpointing Capability = "file_checkpointing"
	CapabilityFileInputs        Capability = "file_inputs"
	CapabilityHooks             Capability = "hooks"
	CapabilityInteractive       Capability = "interactive"
	CapabilityMCP               Capability = "mcp"
	CapabilityModels            Capability = "models"
	CapabilityNativeControl     Capability = "native_control"
	CapabilityPartialMessages   Capability = "partial_messages"
	CapabilityPermissions       Capability = "permissions"
	CapabilityPlugins           Capability = "plugins"
	CapabilitySessionFork       Capability = "session_fork"
	CapabilitySessions          Capability = "sessions"
	CapabilitySkills            Capability = "skills"
	CapabilitySubscriptionUsage Capability = "subscription_usage"
	CapabilitySystemPromptState Capability = "system_prompt_snapshot"
	CapabilityTools             Capability = "tools"
)

// capabilityMeanings describes each capability. The API document and docs/api.md list them.
var capabilityMeanings = map[Capability]string{
	CapabilityAuth:              "Native login, login status and logout through the API",
	CapabilityCustomAgents:      "Subagents defined per run",
	CapabilityDebugOutput:       "Native debug output on request",
	CapabilityDiagnostics:       "GET /v1/agent/diagnostics",
	CapabilityFileCheckpointing: "Native file checkpoints for workspace runs",
	CapabilityFileInputs:        "Provider files downloaded into the workspace at run start",
	CapabilityHooks:             "Native hooks",
	CapabilityInteractive:       "Runs that accept further messages (POST /v1/runs)",
	CapabilityMCP:               "MCP servers",
	CapabilityModels:            "GET /v1/agent/models",
	CapabilityNativeControl:     "Native control requests in both directions",
	CapabilityPartialMessages:   "Partial output streamed as live chunks",
	CapabilityPermissions:       "Tool permission requests (tools.permissions: ask)",
	CapabilityPlugins:           "Native plugins",
	CapabilitySessionFork:       "Forking a session",
	CapabilitySessions:          "Persisted sessions that later runs continue",
	CapabilitySkills:            "Native skills",
	CapabilitySubscriptionUsage: "GET /v1/agent/usage",
	CapabilitySystemPromptState: "Native system prompt snapshots",
	CapabilityTools:             "Native tools in a workspace",
}

var knownCapabilities = slices.Sorted(maps.Keys(capabilityMeanings))

// Meaning describes the capability, or returns "" for an unknown value.
func (c Capability) Meaning() string { return capabilityMeanings[c] }

// KnownCapabilities returns the complete provider-neutral capability vocabulary.
func KnownCapabilities() []Capability { return slices.Clone(knownCapabilities) }
