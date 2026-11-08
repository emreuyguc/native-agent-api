package claude

import (
	"fmt"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Files are per-run files written by the adapter and referenced by arguments.
type Files struct {
	// EmptyMCP is an empty MCP config used to disable MCP servers for stateless runs.
	EmptyMCP string
	// ClientMCP holds options.mcp_config when set.
	ClientMCP string
}

// Arguments returns the CLI arguments for a validated request and its parsed options.
//
// Workspace runs use the full native Claude Code experience: built-in tools, skills, hooks,
// MCP and user/project/local settings. Permission prompts are bypassed unless the request
// asks for them (agent.PermissionsAsk). Stateless runs are plain model calls.
func Arguments(r agent.Request, o RequestOptions, sandbox agent.SandboxMode, files Files) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--replay-user-messages",
		"--model", r.Model,
		"--effort", r.Effort,
		"--settings", o.settings(sandbox, r.AddDirs),
		"--no-chrome",
		// Partial events are live-only; readers opt in with ?partial=true.
		"--include-partial-messages",
	}
	args = append(args, o.flags()...)

	if r.Stateless() {
		// Plain model call: no tools, no MCP, no settings and nothing persisted.
		return append(args,
			"--restricted",
			"--tools", "",
			"--strict-mcp-config",
			"--mcp-config", files.EmptyMCP,
			"--setting-sources", "",
			"--permission-mode", "dontAsk",
			"--permission-prompts", "none",
			"--no-session-persistence",
		)
	}

	switch r.SessionMode {
	case agent.SessionNew:
		args = append(args, "--session-id", r.SessionID)
	case agent.SessionResume:
		args = append(args, "--resume", r.SessionID)
	case agent.SessionFork:
		args = append(args, "--resume", r.ForkFrom, "--fork-session", "--session-id", r.SessionID)
	}

	tools := "default"
	if o.Tools != nil {
		tools = strings.Join(*o.Tools, ",")
	}
	args = append(args, "--tools", tools, "--setting-sources", "user,project,local")
	if files.ClientMCP != "" {
		args = append(args, "--mcp-config", files.ClientMCP)
	}
	if o.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	// One flag per value: these flags are variadic and would otherwise swallow the next one.
	for _, dir := range r.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	for _, dir := range o.PluginDirs {
		args = append(args, "--plugin-dir", dir)
	}
	for _, url := range o.PluginURLs {
		args = append(args, "--plugin-url", url)
	}
	if r.Permissions == agent.PermissionsAsk {
		// Prompts arrive as control_request messages on stdout; answers go to stdin. Without
		// permission_mode the CLI keeps its own default mode.
		if o.PermissionMode != "" {
			args = append(args, "--permission-mode", o.PermissionMode)
		}
		return append(args, "--permission-prompts", "host", "--permission-prompt-tool", "stdio")
	}

	return append(args, "--permission-mode", "bypassPermissions", "--permission-prompts", "none")
}

// Environment returns the complete maintenance-command environment. The gateway's own
// environment (including its API token) is never inherited.
func Environment(opts Options) []string {
	env := []string{
		"PATH=" + opts.Path,
		"HOME=" + opts.HomeDir,
		"TMPDIR=" + opts.TempDir,
		"TERM=dumb",
		"LANG=C.UTF-8",
		"NO_COLOR=1",
		"DISABLE_UPDATES=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"USER=" + opts.User,
		// Never set CLAUDE_CODE_SKIP_PROMPT_HISTORY: it also disables session transcripts,
		// which breaks --resume. Stateless runs use --no-session-persistence instead.
	}
	env = append(env, opts.ProxyEnv...)
	// The native profile must not set CLAUDE_CONFIG_DIR, even to ~/.claude: setting it changes
	// the macOS Keychain entry Claude reads its login from.
	if dir, custom, err := opts.profile(); err == nil && custom {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}

	return env
}

// RunEnvironment adds explicit per-request run settings.
func RunEnvironment(opts Options, o RequestOptions) []string {
	env := Environment(opts)
	if o.MaxOutputTokens > 0 {
		env = append(env, fmt.Sprintf("CLAUDE_CODE_MAX_OUTPUT_TOKENS=%d", o.MaxOutputTokens))
	}
	switch o.FileCheckpointing {
	case "on":
		env = append(env, "CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=1")
	case "off":
		env = append(env, "CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=0")
	}

	return env
}
