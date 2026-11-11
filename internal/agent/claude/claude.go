package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/proctree"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// maxErrorMessage bounds CLI output copied into API error messages.
const maxErrorMessage = 2_000

const (
	// Name is the agent identifier used in the API.
	Name = "claude"
	// Binary is the native executable name resolved through PATH.
	Binary = "claude"
)

// Descriptor describes this adapter. docker/claude/Dockerfile installs TestedVersion; a test in
// internal/agent/builtin keeps both equal.
var Descriptor = agent.Info{
	Name:     Name,
	Title:    "Claude Code",
	Provider: "Anthropic",
	Binary:   Binary,
	Capabilities: []agent.Capability{
		agent.CapabilityAuth,
		agent.CapabilityCustomAgents,
		agent.CapabilityDebugOutput,
		agent.CapabilityDiagnostics,
		agent.CapabilityFileCheckpointing,
		agent.CapabilityFileInputs,
		agent.CapabilityHooks,
		agent.CapabilityInteractive,
		agent.CapabilityMCP,
		agent.CapabilityModels,
		agent.CapabilityNativeControl,
		agent.CapabilityPartialMessages,
		agent.CapabilityPermissions,
		agent.CapabilityPlugins,
		agent.CapabilitySessionFork,
		agent.CapabilitySessions,
		agent.CapabilitySkills,
		agent.CapabilitySubscriptionUsage,
		agent.CapabilitySystemPromptState,
		agent.CapabilityTools,
	},
	LoginMethods:  []string{LoginClaudeAI, LoginConsole},
	TestedVersion: "2.1.318",
	Docs:          "docs/agents/claude-code.md",
}

// Options configures the Claude adapter.
type Options struct {
	Binary string
	// ProfileDir is agent.ProfileNative (Claude's own ~/.claude and Keychain entry) or a custom
	// absolute directory exported as CLAUDE_CONFIG_DIR.
	ProfileDir string
	HomeDir    string
	// User is exported as USER (the macOS Keychain lookup needs it).
	User string
	// WorkDir is the working directory for maintenance commands.
	WorkDir string
	// TempDir hosts per-run scratch directories.
	TempDir string
	// ProxyEnv are the gateway's standard proxy variables (NAME=value), passed on unchanged.
	ProxyEnv []string
	// Path is the PATH given to the CLI.
	Path string
	// SandboxMode selects the CLI's own OS sandbox or an external isolation boundary.
	SandboxMode    agent.SandboxMode
	MaxOutputBytes int64
	CommandTimeout time.Duration
	HTTPClient     *http.Client
	Logger         *slog.Logger
}

// Agent runs the native Claude Code CLI. It implements agent.Agent and agent.UsageReporter.
type Agent struct {
	opts Options
}

var (
	_ agent.Agent               = (*Agent)(nil)
	_ agent.DiagnosticsReporter = (*Agent)(nil)
	_ agent.ModelReporter       = (*Agent)(nil)
	_ agent.UsageReporter       = (*Agent)(nil)
)

// New returns a Claude adapter from explicit runtime settings. The executable name is an
// adapter property; Binary exists only as an internal test seam.
func New(opts Options) (*Agent, error) {
	if opts.Binary == "" {
		opts.Binary = Binary
	}
	if opts.HomeDir == "" || opts.User == "" || opts.WorkDir == "" ||
		opts.TempDir == "" || opts.Path == "" || opts.MaxOutputBytes <= 0 || opts.CommandTimeout <= 0 {
		return nil, errors.New("claude adapter requires explicit runtime paths, identity, output limit, and command timeout")
	}
	if _, _, err := opts.profile(); err != nil {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	if !opts.SandboxMode.Valid() {
		return nil, fmt.Errorf("claude adapter requires sandbox %q or %q, got %q", agent.SandboxCLI, agent.SandboxExternal, opts.SandboxMode)
	}
	if opts.Logger == nil {
		return nil, errors.New("claude adapter requires a logger")
	}
	opts.Logger = opts.Logger.With("agent", Name)
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: opts.CommandTimeout}
	}

	return &Agent{opts: opts}, nil
}

// Info implements agent.Agent.
func (*Agent) Info() agent.Info { return Descriptor }

// Name implements agent.Agent.
func (*Agent) Name() string { return Name }

// Validate implements agent.Agent.
func (*Agent) Validate(req *agent.Request) error {
	if _, err := ParseOptions(req); err != nil {
		return err
	}
	// Efforts are validated against the native model catalog by the server.
	return nil
}

// Version implements agent.Agent.
func (a *Agent) Version(ctx context.Context) (string, error) {
	return a.exec(ctx, "--version")
}

// Diagnostics implements agent.DiagnosticsReporter with Claude's read-only doctor command.
func (a *Agent) Diagnostics(ctx context.Context) (agent.Diagnostics, error) {
	output, err := a.exec(ctx, "doctor")
	if err != nil {
		return agent.Diagnostics{}, err
	}

	return agent.Diagnostics{Format: "text", Output: output}, nil
}

// AuthStatus implements agent.Agent with `claude auth status`, which prints its JSON status
// and exits non-zero when logged out.
func (a *Agent) AuthStatus(ctx context.Context) (agent.AuthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	cmd := a.command(ctx, "auth", "status")
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stdout, stderr := iox.NewTailBuffer(128_000), iox.NewTailBuffer(16_000)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	status, err := decodeAuthStatus([]byte(stdout.String()))
	if err != nil {
		if runErr != nil {
			return agent.AuthStatus{}, errors.Join(agent.ErrUpstream, errors.New(redact.Message(stderr.String(), maxErrorMessage)))
		}
		return agent.AuthStatus{}, err
	}

	return status, nil
}

func decodeAuthStatus(document []byte) (agent.AuthStatus, error) {
	var native struct {
		LoggedIn         *bool  `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(document, &native); err != nil || native.LoggedIn == nil {
		return agent.AuthStatus{}, errors.Join(agent.ErrUpstream, errors.New("claude returned an invalid authentication status"))
	}
	status := agent.AuthStatus{LoggedIn: *native.LoggedIn}
	var fields AuthFields
	if status.LoggedIn {
		status.Account = &agent.Account{Email: native.Email, Plan: native.SubscriptionType}
		fields.Method = native.AuthMethod
	}
	status.Provider = agent.AuthProvider{Provider: agent.NewProvider(Name, fields, document)}

	return status, nil
}

// Interactive implements agent.Agent.
func (a *Agent) Interactive(ctx context.Context, stdio agent.IO, command agent.InteractiveCommand) error {
	return RunInteractive(ctx, a.opts, stdio, command)
}

// RunInteractive runs a native authentication command using only its required process settings.
func RunInteractive(ctx context.Context, opts Options, stdio agent.IO, command agent.InteractiveCommand) error {
	if opts.Binary == "" || opts.HomeDir == "" || opts.User == "" ||
		opts.WorkDir == "" || opts.TempDir == "" || opts.Path == "" {
		return errors.New("claude interactive command requires explicit binary, paths, and identity")
	}
	if _, _, err := opts.profile(); err != nil {
		return fmt.Errorf("claude interactive command: %w", err)
	}
	var args []string
	switch command {
	case agent.CommandLogin:
		args = []string{"auth", "login"}
	case agent.CommandStatus:
		args = []string{"auth", "status", "--text"}
	default:
		return agent.ErrUnsupported
	}
	cmd := exec.CommandContext(ctx, opts.Binary, args...) //nolint:gosec // G204: the configured agent CLI with fixed auth arguments, no shell.
	cmd.Dir = opts.WorkDir
	cmd.Env = Environment(opts)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr

	return cmd.Run()
}

// exec runs a short maintenance command and returns its stdout.
func (a *Agent) exec(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	cmd := a.command(ctx, args...)
	cmd.Dir = a.opts.WorkDir
	cmd.Env = Environment(a.opts)
	stdout, stderr := iox.NewTailBuffer(128_000), iox.NewTailBuffer(16_000)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		message := redact.Message(stderr.String(), maxErrorMessage)
		if message == "" {
			message = "claude command failed"
		}
		a.opts.Logger.WarnContext(ctx, "agent command failed", "command", args[0], "err", err, "stderr", message)

		return "", errors.New(message)
	}

	return stdout.String(), nil
}

// command builds a CLI process; cancelling ctx kills it with every tool, background task and MCP
// server it started (see proctree).
func (a *Agent) command(ctx context.Context, args ...string) *exec.Cmd {
	return proctree.Command(ctx, a.opts.Binary, args...)
}

// nativeProfile is Claude's own profile directory under HOME.
const nativeProfile = ".claude"

// profile returns the effective profile directory and whether it is custom.
func (o Options) profile() (string, bool, error) {
	return agent.ResolveProfile(o.ProfileDir, o.HomeDir, nativeProfile)
}

// ProfileDir implements agent.ProfileReporter.
func (a *Agent) ProfileDir() string {
	dir, _, _ := a.opts.profile()

	return dir
}
