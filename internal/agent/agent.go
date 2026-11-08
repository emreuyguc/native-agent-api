// Package agent defines the contract between the gateway and native coding agent CLIs.
//
// Each adapter runs its CLI with the
// CLI's own native protocol. The gateway only sees the common Request / Event / Result
// model below; native events are always forwarded untouched in Event.Raw so no
// agent-specific feature is lost.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"
)

// Errors shared by adapters. The HTTP layer maps them to status codes with errors.Is.
var (
	ErrInvalidRequest = errors.New("invalid request")
	ErrLoginRequired  = errors.New("agent login is required")
	ErrLoginExpired   = errors.New("agent login expired; run the login command again")
	ErrForbidden      = errors.New("the agent login does not grant this operation")
	ErrUnsupported    = errors.New("operation is not supported by this agent")
	ErrUpstream       = errors.New("agent upstream request failed")
)

// Info describes an adapter. It is static and safe to expose over the API.
//
// Terminology: the *agent* is the native CLI (Claude Code), the *provider* is the vendor
// behind it (Anthropic), and the *adapter* is the gateway code
// that drives the CLI.
type Info struct {
	// Name is the stable identifier used in the API and configuration ("claude").
	Name string `json:"name"`
	// Title is the product name of the CLI ("Claude Code").
	Title string `json:"title"`
	// Provider is the vendor of the CLI ("Anthropic").
	Provider string `json:"provider"`
	// Binary is the executable the adapter starts.
	Binary string `json:"binary"`
	// Capabilities lists native CLI surfaces implemented by this adapter. It is sorted and
	// contains no duplicates. Gateway-wide REST, streaming, file and workspace features are
	// described by the OpenAPI document instead.
	Capabilities []Capability `json:"capabilities"`
	// LoginMethods lists the native login flows accepted by POST /v1/agent/login/start.
	LoginMethods []string `json:"login_methods"`
	// TestedVersion is the CLI version the adapter is verified against.
	TestedVersion string `json:"tested_version"`
	// Docs points to the support matrix in the repository.
	Docs string `json:"docs"`
}

// Agent is a native coding agent CLI.
type Agent interface {
	// Info describes the adapter.
	Info() Info
	// Name is Info().Name.
	Name() string
	// Validate applies agent-specific checks to an already normalized request.
	// Failures wrap ErrInvalidRequest.
	Validate(req *Request) error
	// Start launches the CLI, sends req.Prompt as the first message and returns the live
	// conversation. Events are passed to onEvent from a single goroutine, in order; returning
	// an error from onEvent aborts the conversation. Cancelling ctx kills the process.
	Start(ctx context.Context, req Request, onEvent EventFunc) (Conversation, error)
	// Version returns the CLI version string.
	Version(ctx context.Context) (string, error)
	// AuthStatus reports the native login state. Being logged out is a result, not an error.
	AuthStatus(ctx context.Context) (AuthStatus, error)
	// Interactive runs a CLI maintenance flow (login, status) on a terminal.
	Interactive(ctx context.Context, stdio IO, command InteractiveCommand) error
}

// UsageReporter is implemented by agents that can report subscription usage.
type UsageReporter interface {
	Usage(ctx context.Context) (Usage, error)
}

// AuthStatus is the native login state.
type AuthStatus struct {
	LoggedIn bool `json:"logged_in"`
	// Account is present when logged in.
	Account  *Account     `json:"account,omitempty"`
	Provider AuthProvider `json:"provider"`
}

// Account is the logged-in account.
type Account struct {
	Email string `json:"email,omitempty" doc:"Account e-mail."`
	Plan  string `json:"plan,omitempty"  doc:"Subscription plan as the agent names it, for display (e.g. pro); absent for API-key logins."`
}

// Usage is the subscription allowance.
type Usage struct {
	Windows  []UsageWindow        `json:"windows"`
	Provider SubscriptionProvider `json:"provider"`
}

// UsageWindow is one rate-limit window of the subscription.
type UsageWindow struct {
	ID          string     `json:"id" doc:"Stable window identifier as the agent reports it (e.g. session, weekly)."`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at,omitempty" doc:"When the window resets; absent while it has no usage."`
}

// ModelReporter is implemented only when a native CLI can discover its available models.
type ModelReporter interface {
	Models(ctx context.Context) ([]Model, error)
}

// Model is one model the native CLI reports.
type Model struct {
	ID   string `json:"id"   doc:"Value for model.id."`
	Name string `json:"name" doc:"Display name."`
	// Efforts are the reasoning efforts the model accepts; requests are validated against them.
	Efforts  []string      `json:"efforts"`
	Provider ModelProvider `json:"provider"`
}

// DiagnosticsReporter is implemented by agents with a bounded, non-interactive native health
// check.
type DiagnosticsReporter interface {
	Diagnostics(ctx context.Context) (Diagnostics, error)
}

// Diagnostics is the native diagnostic report.
type Diagnostics struct {
	Format string `json:"format" enum:"text,json" doc:"text: a plain report; json: output holds a JSON document."`
	Output string `json:"output"`
}

// InteractiveCommand names a terminal flow supported by Agent.Interactive.
type InteractiveCommand string

// Interactive commands.
const (
	CommandLogin  InteractiveCommand = "login"
	CommandStatus InteractiveCommand = "status"
)

// IO is the terminal used by interactive commands.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Conversation is a live CLI process. A conversation has one or more turns; each turn ends
// with an Event carrying a Result. Methods are safe for concurrent use.
type Conversation interface {
	// Send queues a user message. A message sent during a turn starts the next turn.
	Send(text string) error
	// Interrupt stops the current turn; the conversation stays open.
	Interrupt() error
	// Answer resolves a pending permission request.
	Answer(answer PermissionAnswer) error
	// Control sends a native control request (a JSON object) to the agent and waits for its
	// response. Agents without a control protocol return ErrUnsupported.
	Control(ctx context.Context, request json.RawMessage) (json.RawMessage, error)
	// Respond answers a control request the agent sent (Event.Control). Exactly one of
	// response and errMessage is used.
	Respond(id string, response json.RawMessage, errMessage string) error
	// CloseInput ends the conversation after the current turn; the process then exits.
	CloseInput() error
	// Wait blocks until the process has exited. It returns nil for a clean exit.
	Wait() error
}

// NativeSession is implemented by conversations backed by a provider-owned persistent session.
type NativeSession interface {
	NativeSessionID() string
}

// Errors returned by Conversation methods.
var (
	ErrInputClosed       = errors.New("conversation input is closed")
	ErrUnknownPermission = errors.New("permission request is not pending")
	ErrUnknownControl    = errors.New("control request is not pending")
	// ErrControlRejected wraps the agent's error answer to a control request.
	ErrControlRejected = errors.New("agent rejected the control request")
)

// Event is one event emitted by the CLI. Native events are forwarded unmodified in Raw; the
// typed fields are set by the adapter for the events the gateway itself must understand.
type Event struct {
	// Type is the native event type (e.g. Claude "assistant", "result").
	Type string
	// Raw is the unmodified native JSON event.
	Raw json.RawMessage
	// TurnStarted marks the first event of a turn.
	TurnStarted bool
	// Result is set on the terminal event of a turn.
	Result *Result
	// Permission is set when the agent asks whether it may use a tool. Raw is then the
	// native protocol message and is not forwarded to clients.
	Permission *PermissionRequest
	// Control is set when the agent sends any other control request that the client must
	// answer with Conversation.Respond (e.g. Claude hook callbacks registered by the client).
	Control *ControlRequest
	// Partial marks incremental output (token deltas). Partial events are delivered to live
	// readers but not stored: the complete message follows as a normal event.
	Partial bool
	// Diagnostic is bounded, redacted adapter diagnostic output requested by the client. It is
	// a gateway event, not a native event.
	Diagnostic string
	// Observation is optional structured lifecycle data for a native task, subagent, or
	// background process. Raw remains the lossless native event.
	Observation *Observation
}

// Observation is the lifecycle of a native task, subagent or background process. An update
// may leave Kind, Label and ParentID empty to keep the known values.
type Observation struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"  enum:"task,subagent,process"`
	State    string `json:"state" enum:"active,completed,failed,cancelled"`
	Label    string `json:"label,omitempty"     doc:"Human-readable description."`
	ParentID string `json:"parent_id,omitempty" doc:"Observation that started this one."`
}

// Observation kinds and states.
const (
	ObservationTask     = "task"
	ObservationSubagent = "subagent"
	ObservationProcess  = "process"

	ObservationActive    = "active"
	ObservationCompleted = "completed"
	ObservationFailed    = "failed"
	ObservationCancelled = "cancelled"
)

// ControlRequest is a native control request from the agent that the client answers. It has
// no common content: what it asks is in the provider section.
type ControlRequest struct {
	ID       string          `json:"control_id"`
	Provider ControlProvider `json:"provider"`
}

// EventFunc receives events while a conversation is in progress.
type EventFunc func(Event) error

// Outcome is how a turn ended.
type Outcome string

// Outcomes.
const (
	// OutcomeCompleted: the agent answered.
	OutcomeCompleted Outcome = "completed"
	// OutcomeFailed: the turn ended with an error.
	OutcomeFailed Outcome = "failed"
	// OutcomeInterrupted: the client interrupted the turn.
	OutcomeInterrupted Outcome = "interrupted"
	// OutcomeLimitReached: a configured turn, budget or retry limit ended the turn.
	OutcomeLimitReached Outcome = "limit_reached"
)

// Result is the outcome of one turn.
type Result struct {
	// Text is the agent's answer; for a turn that did not complete, whatever it answered
	// before it stopped (often empty).
	Text    string  `json:"text"`
	Outcome Outcome `json:"outcome" enum:"completed,failed,interrupted,limit_reached"`
	// Error says why the turn did not complete (redacted); set exactly when outcome is not
	// completed.
	Error string `json:"error,omitempty"`
	// Tokens are the turn's token counts; absent when the agent reported none.
	Tokens *Tokens `json:"tokens,omitempty"`
	// Context is how full the model's context window is after the turn; absent when the turn
	// made no model call.
	Context *ContextUsage `json:"context,omitempty"`
	// Provider holds the native outcome code, the agent's own usage data and the native result.
	Provider ResultProvider `json:"provider"`
	// AgentInitiated marks a native turn the agent started on its own (Claude Code reacting to
	// a background-task notification), not an answer to a client message. The run forwards it
	// as a native event but does not count it as a turn or replace the run result.
	AgentInitiated bool `json:"-"`
}

// ContextUsage is the occupancy of the model's context window.
type ContextUsage struct {
	Window int64 `json:"window" doc:"Context window of the turn's model, in tokens."`
	Used   int64 `json:"used"   doc:"Input tokens of the turn's last model call (cached included): what the model holds in context."`
}

// Failed reports whether the turn did not complete.
func (r *Result) Failed() bool { return r.Outcome != OutcomeCompleted }

// Tokens is a token breakdown. A nil field was not reported, which is not the same as zero.
// Input is the uncached input; cached and cache-write input are counted only in CacheRead and
// CacheCreation, for every agent. A turn's counts cover all of its model calls.
type Tokens struct {
	Input         *int64 `json:"input,omitempty"`
	Output        *int64 `json:"output,omitempty"`
	CacheRead     *int64 `json:"cache_read,omitempty"`
	CacheCreation *int64 `json:"cache_creation,omitempty"`
}

// Fields returns pointers to the four counts, for summing.
func (t *Tokens) Fields() [4]**int64 {
	return [4]**int64{&t.Input, &t.Output, &t.CacheRead, &t.CacheCreation}
}

// TokenFields are the JSON names of the Tokens fields, in Fields order.
var TokenFields = [4]string{"input", "output", "cache_read", "cache_creation"}

// UsageTotal sums the token counts of the turns the gateway observed.
type UsageTotal struct {
	Turns  UsageTurns `json:"turns"`
	Tokens Tokens     `json:"tokens"`
	// Partial lists the token counts that at least one reported turn did not include; their
	// sums cover fewer turns than turns.reported. Turns without any counts are in
	// turns.unreported instead.
	Partial []string `json:"partial" enum:"input,output,cache_read,cache_creation"`
}

// UsageTurns counts the observed turns.
type UsageTurns struct {
	Reported   int `json:"reported"   doc:"Turns that reported token counts."`
	Unreported int `json:"unreported" doc:"Turns without token counts."`
}

// Add folds one turn's tokens (nil: unreported) into the total.
func (u *UsageTotal) Add(tokens *Tokens) {
	if tokens == nil {
		u.Turns.Unreported++
		return
	}
	u.Turns.Reported++
	source := *tokens
	targets, values := u.Tokens.Fields(), source.Fields()
	for i, target := range targets {
		value := *values[i]
		if value == nil {
			u.markPartial(TokenFields[i])
			continue
		}
		sum := *value
		if *target != nil {
			sum += **target
		}
		*target = &sum
	}
}

// Merge folds another total into u.
func (u *UsageTotal) Merge(other UsageTotal) {
	u.Turns.Reported += other.Turns.Reported
	u.Turns.Unreported += other.Turns.Unreported
	for _, field := range other.Partial {
		u.markPartial(field)
	}
	targets, values := u.Tokens.Fields(), other.Tokens.Fields()
	for i, target := range targets {
		if value := *values[i]; value != nil {
			sum := *value
			if *target != nil {
				sum += **target
			}
			*target = &sum
		}
	}
}

// markPartial adds field to Partial, keeping the TokenFields order.
func (u *UsageTotal) markPartial(field string) {
	if slices.Contains(u.Partial, field) {
		return
	}
	partial := make([]string, 0, len(u.Partial)+1)
	for _, name := range TokenFields {
		if name == field || slices.Contains(u.Partial, name) {
			partial = append(partial, name)
		}
	}
	u.Partial = partial
}

// Clone returns a deep copy.
func (u UsageTotal) Clone() UsageTotal {
	cloned := u
	cloned.Partial = append([]string{}, u.Partial...)
	cloned.Tokens = Tokens{}
	targets, values := cloned.Tokens.Fields(), u.Tokens.Fields()
	for i, target := range targets {
		if value := *values[i]; value != nil {
			copied := *value
			*target = &copied
		}
	}

	return cloned
}

// NewUsageTotal returns an empty total.
func NewUsageTotal() UsageTotal { return UsageTotal{Partial: []string{}} }

// Permission kinds: what the agent asks to do. Session and workspace rules match by kind.
const (
	PermissionCommand    = "command"
	PermissionFileChange = "file_change"
	PermissionMCP        = "mcp"
	PermissionOther      = "other"
)

// PermissionKinds lists every kind.
var PermissionKinds = []string{PermissionCommand, PermissionFileChange, PermissionMCP, PermissionOther}

// PermissionRequest asks the client whether the agent may do something.
type PermissionRequest struct {
	ID      string `json:"permission_id"`
	Kind    string `json:"kind"    enum:"command,file_change,mcp,other"`
	Summary string `json:"summary" doc:"What the agent wants to do: the command, the file, or the tool."`
	// Provider holds the native tool or method and the unmodified native request.
	Provider PermissionProvider `json:"provider"`
}

// PermissionAnswer resolves a PermissionRequest.
type PermissionAnswer struct {
	ID    string
	Allow bool
	// Message explains a denial to the agent.
	Message string
	// Options are the agent's own fields of the allow request's provider section (without
	// name), validated by the adapter; nil when none were sent.
	Options json.RawMessage
}

// SandboxMode selects who isolates the commands an agent runs.
type SandboxMode string

const (
	// SandboxCLI enables the CLI's own OS sandbox (Seatbelt on macOS, bubblewrap on
	// Linux). Runs fail instead of executing unconfined when that sandbox is unavailable.
	SandboxCLI SandboxMode = "cli"
	// SandboxExternal disables the CLI's own sandbox because an outer boundary, such as a
	// hardened container, already isolates the process.
	SandboxExternal SandboxMode = "external"
)

// Valid reports whether s is a supported sandbox mode.
func (s SandboxMode) Valid() bool { return s == SandboxCLI || s == SandboxExternal }

// Authenticator is implemented by agents whose native login flow can be driven over the API.
// The context passed to StartLogin bounds the whole login; cancelling it ends the flow.
type Authenticator interface {
	StartLogin(ctx context.Context, req LoginRequest) (Login, error)
	Logout(ctx context.Context) error
}

// LoginRequest selects one native login method from Info.LoginMethods.
type LoginRequest struct {
	Method string
	// Options are the agent's own fields of the login request's provider section (without
	// name), validated by the adapter; nil when none were sent.
	Options json.RawMessage
}

// Login flows: what the user does to finish a login.
const (
	// FlowPasteCode: open the URL; the page shows a code the user submits to the gateway.
	FlowPasteCode = "paste_code"
	// FlowDeviceCode: open the URL and enter the user code shown by the gateway.
	FlowDeviceCode = "device_code"
	// FlowBrowser: open the URL in a browser on the gateway host; nothing to submit.
	FlowBrowser = "browser"
)

// LoginChallenge is what the user must do to finish a login.
type LoginChallenge struct {
	// Flow is one of the Flow* constants.
	Flow string
	// URL is the page the user opens in a browser.
	URL string
	// UserCode is the code the user enters on the page (FlowDeviceCode only).
	UserCode string
}

// Login is one running native login flow.
type Login interface {
	Challenge() LoginChallenge
	// SubmitCode passes the code shown by the sign-in page to the native CLI.
	SubmitCode(code string) error
	// Wait blocks until the flow ends and returns nil only when the login succeeded.
	Wait() error
	// Cancel stops the flow; Wait then returns an error.
	Cancel()
}

// MaxLoginCode bounds a code pasted from a sign-in page.
const MaxLoginCode = 4096

// ProfileNative selects the native CLI's own default profile location under HOME.
const ProfileNative = "native"

// ResolveProfile validates an agent profile setting. It returns the effective profile
// directory and whether it is a custom location that must be passed to the CLI. A custom
// directory equal to the native default is rejected: some CLIs change behavior merely because
// the location is set explicitly (Claude's macOS Keychain entry name, for example).
func ResolveProfile(profileDir, home, nativeName string) (dir string, custom bool, err error) {
	nativeDir := filepath.Join(home, nativeName)
	switch {
	case profileDir == ProfileNative:
		return nativeDir, false, nil
	case !filepath.IsAbs(profileDir):
		return "", false, fmt.Errorf("profile directory must be %q or an absolute path, got %q", ProfileNative, profileDir)
	case filepath.Clean(profileDir) == nativeDir:
		return "", false, fmt.Errorf("profile directory %s is the CLI's native profile; use %q", profileDir, ProfileNative)
	default:
		return filepath.Clean(profileDir), true, nil
	}
}

// ProfileReporter is implemented by adapters to report their effective profile directory, so
// the gateway can keep it out of workspaces and show it to operators.
type ProfileReporter interface {
	ProfileDir() string
}
