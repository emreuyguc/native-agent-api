// Package codex adapts the native Codex CLI app-server protocol to the common agent contract.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/proctree"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

const (
	Name    = "codex"
	Binary  = "codex"
	version = "0.167.0"

	maxErrorMessage = 2_000
)

var Descriptor = agent.Info{
	Name:     Name,
	Title:    "Codex CLI",
	Provider: "OpenAI",
	Binary:   Binary,
	Capabilities: []agent.Capability{
		agent.CapabilityAuth,
		agent.CapabilityCustomAgents,
		agent.CapabilityDiagnostics,
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
		agent.CapabilityTools,
	},
	LoginMethods:  []string{LoginChatGPTDeviceCode, LoginChatGPT},
	TestedVersion: version,
	Docs:          "docs/agents/codex-cli.md",
}

type Options struct {
	Binary string
	// ProfileDir is agent.ProfileNative (Codex's own ~/.codex) or a custom absolute directory
	// exported as CODEX_HOME.
	ProfileDir     string
	HomeDir        string
	User           string
	WorkDir        string
	TempDir        string
	ProxyEnv       []string // Standard proxy variables (NAME=value), passed on unchanged.
	Path           string
	SandboxMode    agent.SandboxMode
	MaxOutputBytes int64
	CommandTimeout time.Duration
	Logger         *slog.Logger
}

type Agent struct{ opts Options }

var (
	_ agent.Agent               = (*Agent)(nil)
	_ agent.DiagnosticsReporter = (*Agent)(nil)
	_ agent.ModelReporter       = (*Agent)(nil)
	_ agent.UsageReporter       = (*Agent)(nil)
)

func New(opts Options) (*Agent, error) {
	if opts.Binary == "" {
		opts.Binary = Binary
	}
	if opts.HomeDir == "" || opts.User == "" || opts.WorkDir == "" ||
		opts.TempDir == "" || opts.Path == "" || opts.MaxOutputBytes <= 0 || opts.CommandTimeout <= 0 {
		return nil, errors.New("codex adapter requires explicit runtime paths, identity, output limit, and command timeout")
	}
	if _, _, err := opts.profile(); err != nil {
		return nil, fmt.Errorf("codex adapter: %w", err)
	}
	if !opts.SandboxMode.Valid() {
		return nil, fmt.Errorf("codex adapter requires sandbox %q or %q, got %q", agent.SandboxCLI, agent.SandboxExternal, opts.SandboxMode)
	}
	if opts.Logger == nil {
		return nil, errors.New("codex adapter requires a logger")
	}
	opts.Logger = opts.Logger.With("agent", Name)

	return &Agent{opts: opts}, nil
}

func (*Agent) Info() agent.Info { return Descriptor }

func (*Agent) Name() string { return Name }

func (*Agent) Validate(req *agent.Request) error {
	if _, err := ParseOptions(req); err != nil {
		return err
	}

	return nil
}

func (a *Agent) Version(ctx context.Context) (string, error) { return a.exec(ctx, "--version") }

func (a *Agent) Diagnostics(ctx context.Context) (agent.Diagnostics, error) {
	output, err := a.exec(ctx, "doctor", "--json")
	if err != nil {
		return agent.Diagnostics{}, err
	}

	return agent.Diagnostics{Format: "json", Output: output}, nil
}

// AuthStatus implements agent.Agent with app-server account/read.
func (a *Agent) AuthStatus(ctx context.Context) (agent.AuthStatus, error) {
	result, err := a.rpc(ctx, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return agent.AuthStatus{}, err
	}

	return decodeAccount(result)
}

func decodeAccount(document json.RawMessage) (agent.AuthStatus, error) {
	var native struct {
		Account *struct {
			Type     string  `json:"type"`
			Email    *string `json:"email"`
			PlanType *string `json:"planType"`
		} `json:"account"`
	}
	if err := json.Unmarshal(document, &native); err != nil {
		return agent.AuthStatus{}, errors.Join(agent.ErrUpstream, errors.New("codex returned an invalid account document"))
	}
	status := agent.AuthStatus{LoggedIn: native.Account != nil}
	var fields AuthFields
	if account := native.Account; account != nil {
		fields.Method = account.Type
		status.Account = &agent.Account{}
		if account.Email != nil {
			status.Account.Email = *account.Email
		}
		if account.PlanType != nil {
			status.Account.Plan = *account.PlanType
		}
	}
	status.Provider = agent.AuthProvider{Provider: agent.NewProvider(Name, fields, document)}

	return status, nil
}

func (a *Agent) Interactive(ctx context.Context, stdio agent.IO, command agent.InteractiveCommand) error {
	return RunInteractive(ctx, a.opts, stdio, command)
}

func RunInteractive(ctx context.Context, opts Options, stdio agent.IO, command agent.InteractiveCommand) error {
	if opts.Binary == "" || opts.HomeDir == "" || opts.User == "" ||
		opts.WorkDir == "" || opts.TempDir == "" || opts.Path == "" {
		return errors.New("codex interactive command requires explicit binary, paths, and identity")
	}
	if _, _, err := opts.profile(); err != nil {
		return fmt.Errorf("codex interactive command: %w", err)
	}
	var args []string
	switch command {
	case agent.CommandLogin:
		args = []string{"login"}
	case agent.CommandStatus:
		args = []string{"login", "status"}
	default:
		return agent.ErrUnsupported
	}
	cmd := commandProcess(ctx, opts.Binary, args...)
	cmd.Dir, cmd.Env = opts.WorkDir, Environment(opts)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr

	return cmd.Run()
}

// nativeProfile is Codex's own profile directory under HOME.
const nativeProfile = ".codex"

func (o Options) profile() (string, bool, error) {
	return agent.ResolveProfile(o.ProfileDir, o.HomeDir, nativeProfile)
}

func Environment(opts Options) []string {
	env := []string{
		"HOME=" + opts.HomeDir,
		"PATH=" + opts.Path,
		"TMPDIR=" + opts.TempDir,
		"USER=" + opts.User,
	}
	env = append(env, opts.ProxyEnv...)
	if dir, custom, err := opts.profile(); err == nil && custom {
		env = append(env, "CODEX_HOME="+dir)
	}

	return env
}

func (a *Agent) exec(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	cmd := commandProcess(ctx, a.opts.Binary, args...)
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stdout, stderr := iox.NewTailBuffer(128_000), iox.NewTailBuffer(16_000)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		message := redact.Message(stderr.String(), maxErrorMessage)
		if message == "" {
			message = "codex command failed"
		}
		a.opts.Logger.WarnContext(ctx, "agent command failed", "command", args[0], "err", err, "stderr", message)

		return "", errors.New(message)
	}

	return stdout.String(), nil
}

// commandProcess builds a CLI process; cancelling ctx kills it with everything it started.
func commandProcess(ctx context.Context, binary string, args ...string) *exec.Cmd {
	return proctree.Command(ctx, binary, args...)
}

// ProfileDir implements agent.ProfileReporter.
func (a *Agent) ProfileDir() string {
	dir, _, _ := a.opts.profile()

	return dir
}
