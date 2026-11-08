package agent

import (
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

var knownCapabilities = []Capability{
	CapabilityAuth,
	CapabilityCustomAgents,
	CapabilityDebugOutput,
	CapabilityDiagnostics,
	CapabilityFileCheckpointing,
	CapabilityFileInputs,
	CapabilityHooks,
	CapabilityInteractive,
	CapabilityMCP,
	CapabilityModels,
	CapabilityNativeControl,
	CapabilityPartialMessages,
	CapabilityPermissions,
	CapabilityPlugins,
	CapabilitySessionFork,
	CapabilitySessions,
	CapabilitySkills,
	CapabilitySubscriptionUsage,
	CapabilitySystemPromptState,
	CapabilityTools,
}

// KnownCapabilities returns the complete provider-neutral capability vocabulary.
func KnownCapabilities() []Capability { return slices.Clone(knownCapabilities) }
