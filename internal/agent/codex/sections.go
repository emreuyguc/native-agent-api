package codex

import (
	"reflect"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// AuthFields are Codex's fields of the auth status provider section.
type AuthFields struct {
	Method string `json:"method,omitempty" doc:"Native account type, e.g. chatgpt or apiKey."`
}

// SubscriptionFields are Codex's fields of the subscription usage provider section.
type SubscriptionFields struct {
	WindowMinutes map[string]int64 `json:"window_minutes,omitempty" doc:"Window length in minutes, by window id."`
}

// ResultFields are Codex's fields of a turn result's provider section.
type ResultFields struct {
	Reason string `json:"reason" doc:"Native turn status, e.g. completed, interrupted or failed."`
	// Thread is the native cumulative usage of the thread, when Codex reported it.
	Thread *ThreadUsage `json:"thread,omitempty"`
}

// ThreadUsage is the native cumulative token usage of a thread. It is kept across resumes and
// a fork starts from its parent's.
type ThreadUsage struct {
	Tokens agent.Tokens `json:"tokens"`
}

// RequestFields are Codex's fields of a permission or control request's provider section.
type RequestFields struct {
	Method string `json:"method" doc:"Native app-server request method, e.g. item/commandExecution/requestApproval."`
}

// ProviderSections implements agent.SectionDescriber.
func (*Agent) ProviderSections() map[agent.Section]reflect.Type { return Sections() }

// Sections describes Codex's provider sections.
func Sections() map[agent.Section]reflect.Type {
	return map[agent.Section]reflect.Type{
		agent.SectionRun:          reflect.TypeFor[RequestOptions](),
		agent.SectionAuth:         reflect.TypeFor[AuthFields](),
		agent.SectionSubscription: reflect.TypeFor[SubscriptionFields](),
		agent.SectionResult:       reflect.TypeFor[ResultFields](),
		agent.SectionPermission:   reflect.TypeFor[RequestFields](),
		agent.SectionControl:      reflect.TypeFor[RequestFields](),
	}
}

// outcome maps a native turn status to the gateway outcome.
func outcome(status string) agent.Outcome {
	switch status {
	case "completed":
		return agent.OutcomeCompleted
	case "interrupted":
		return agent.OutcomeInterrupted
	default:
		return agent.OutcomeFailed
	}
}
