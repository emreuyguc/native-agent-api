package claude

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// PermissionModes are the native modes accepted with permissions "ask".
var PermissionModes = []string{"acceptEdits", "auto", "dontAsk", "manual", "plan"}

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	// toolPattern never starts with "-": --allowed-tools and --disallowed-tools are variadic, so
	// such a value would be parsed as another flag (docs/security.md F-3).
	toolPattern     = regexp.MustCompile(`^[A-Za-z0-9_*.:][^\x00-\x1f]{0,255}$`)
	betaPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	fileIDPattern   = regexp.MustCompile(`^file_[A-Za-z0-9_-]{1,128}$`)
	autocompactRule = regexp.MustCompile(`^\d+(?:\.\d+)?[kKmM]?$`)
)

// Limits for native option values.
const (
	maxOptionText  = 256_000
	maxOptionList  = 128
	maxOptionBytes = 256_000
)

// RequestOptions are Claude Code's native per-run options (the request "options" object).
// Each field maps to one CLI flag; see docs/agents/claude-code.md.
type RequestOptions struct {
	// MaxTurns and MaxOutputTokens are optional native limits; omitted means the CLI's own.
	MaxTurns                           int            `json:"max_turns,omitempty"`
	MaxOutputTokens                    int            `json:"max_output_tokens,omitempty"`
	AppendSystemPrompt                 string         `json:"append_system_prompt,omitempty"`
	Agent                              string         `json:"agent,omitempty"`
	Agents                             map[string]any `json:"agents,omitempty"`
	Tools                              *[]string      `json:"tools,omitempty"`
	AllowedTools                       []string       `json:"allowed_tools,omitempty"`
	DisallowedTools                    []string       `json:"disallowed_tools,omitempty"`
	PermissionMode                     string         `json:"permission_mode,omitempty"`
	MaxBudgetUSD                       float64        `json:"max_budget_usd,omitempty"`
	FallbackModel                      string         `json:"fallback_model,omitempty"`
	FileCheckpointing                  string         `json:"file_checkpointing,omitempty"`
	SessionName                        string         `json:"session_name,omitempty"`
	IncludeHookEvents                  bool           `json:"include_hook_events,omitempty"`
	ForwardSubagentText                bool           `json:"forward_subagent_text,omitempty"`
	Autocompact                        string         `json:"autocompact,omitempty"`
	DisableSlashCommands               bool           `json:"disable_slash_commands,omitempty"`
	Debug                              bool           `json:"debug,omitempty"`
	DebugFilter                        string         `json:"debug_filter,omitempty"`
	Bare                               bool           `json:"bare,omitempty"`
	SafeMode                           bool           `json:"safe_mode,omitempty"`
	ExcludeDynamicSystemPromptSections bool           `json:"exclude_dynamic_system_prompt_sections,omitempty"`
	PromptSuggestions                  bool           `json:"prompt_suggestions,omitempty"`
	Brief                              bool           `json:"brief,omitempty"`
	Betas                              []string       `json:"betas,omitempty"`
	AllowDangerouslySkipPermissions    bool           `json:"allow_dangerously_skip_permissions,omitempty"`
	// Files are Anthropic Files API resources downloaded into the workspace at startup
	// ("file_id:relative/path").
	Files []string `json:"files,omitempty"`
	// PluginDirs load plugins from directories inside an allowed root or the workspace.
	PluginDirs []string `json:"plugin_dirs,omitempty"`
	// PluginURLs load plugins from https URLs (zip archives).
	PluginURLs           []string       `json:"plugin_urls,omitempty"`
	MCPConfig            map[string]any `json:"mcp_config,omitempty"`
	StrictMCPConfig      bool           `json:"strict_mcp_config,omitempty"`
	SystemPromptSnapshot string         `json:"system_prompt_snapshot,omitempty"`
	Settings             map[string]any `json:"settings,omitempty"`
	// Initialize is sent as the control protocol "initialize" request before the first
	// message (hooks and SDK MCP servers answered by the client, custom agents, ...).
	Initialize map[string]any `json:"initialize,omitempty"`
}

// statelessOptions are the only options allowed without a workspace: none of them can
// enable tools, MCP servers, hooks or settings.
var statelessOptions = []string{
	"append_system_prompt", "max_budget_usd", "fallback_model", "betas",
	"exclude_dynamic_system_prompt_sections", "autocompact", "system_prompt_snapshot", "file_checkpointing",
	"debug", "debug_filter", "max_turns", "max_output_tokens",
}

// ParseOptions decodes and validates raw options for req.
func ParseOptions(req *agent.Request) (RequestOptions, error) {
	var o RequestOptions
	if len(req.Options) == 0 || string(req.Options) == "null" {
		return o, nil
	}
	if len(req.Options) > maxOptionBytes {
		return o, agent.Invalid("provider exceeds %d bytes", maxOptionBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Options))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&o); err != nil {
		return o, agent.Invalid("provider: %v", err)
	}
	if req.Stateless() {
		var present map[string]json.RawMessage
		_ = json.Unmarshal(req.Options, &present)
		for name := range maps.Keys(present) {
			if !slices.Contains(statelessOptions, name) {
				return o, agent.Invalid("provider.%s needs a workspace (stateless runs allow only %v)", name, statelessOptions)
			}
		}
	}

	err := o.validate(req)

	return o, err
}

func (o *RequestOptions) validate(req *agent.Request) error {
	lists := map[string][]string{"allowed_tools": o.AllowedTools, "disallowed_tools": o.DisallowedTools}
	if o.Tools != nil {
		lists["tools"] = *o.Tools
	}
	for name, list := range lists {
		if len(list) > maxOptionList {
			return agent.Invalid("provider.%s allows at most %d entries", name, maxOptionList)
		}
		for _, tool := range list {
			if !toolPattern.MatchString(tool) {
				return agent.Invalid("provider.%s contains an invalid tool rule %q", name, tool)
			}
		}
	}
	if len(o.Files) > maxOptionList {
		return agent.Invalid("provider.files allows at most %d entries", maxOptionList)
	}
	for _, spec := range o.Files {
		id, path, ok := strings.Cut(spec, ":")
		if !ok || !fileIDPattern.MatchString(id) || !filepath.IsLocal(path) || strings.ContainsAny(path, "\\\x00") {
			return agent.Invalid("provider.files entries must be file_id:relative/path inside the workspace, got %q", spec)
		}
	}
	if len(o.PluginDirs) > agent.MaxAddDirs || len(o.PluginURLs) > agent.MaxAddDirs {
		return agent.Invalid("provider.plugin_dirs and provider.plugin_urls allow at most %d entries each", agent.MaxAddDirs)
	}
	for i, dir := range o.PluginDirs {
		resolved, err := req.Dir("options.plugin_dirs", dir)
		if err != nil {
			return err
		}
		o.PluginDirs[i] = resolved
	}
	for _, raw := range o.PluginURLs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || len(raw) > 2048 {
			return agent.Invalid("provider.plugin_urls entries must be https URLs, got %q", raw)
		}
	}
	for _, beta := range o.Betas {
		if !betaPattern.MatchString(beta) {
			return agent.Invalid("provider.betas contains an invalid value %q", beta)
		}
	}
	switch {
	case len(o.AppendSystemPrompt) > maxOptionText:
		return agent.Invalid("provider.append_system_prompt exceeds %d bytes", maxOptionText)
	case o.Agent != "" && !namePattern.MatchString(o.Agent):
		return agent.Invalid("provider.agent is not a valid agent name")
	case o.SessionName != "" && (len(o.SessionName) > 200 || !toolPattern.MatchString(o.SessionName)):
		return agent.Invalid("provider.session_name must be 1-200 printable characters")
	case o.FallbackModel != "" && !validModelList(o.FallbackModel):
		return agent.Invalid("provider.fallback_model must be a comma-separated list of valid model names")
	case o.MaxBudgetUSD < 0:
		return agent.Invalid("provider.max_budget_usd must not be negative")
	case o.Autocompact != "" && !validAutocompact(o.Autocompact):
		return agent.Invalid("provider.autocompact must be auto or between 100k and 1M tokens")
	case o.PermissionMode != "" && req.Permissions != agent.PermissionsAsk:
		return agent.Invalid("provider.permission_mode needs permissions ask (bypass always skips prompts)")
	case o.PermissionMode != "" && !slices.Contains(PermissionModes, o.PermissionMode):
		return agent.Invalid("provider.permission_mode must be one of %v", PermissionModes)
	case o.SystemPromptSnapshot != "" && o.SystemPromptSnapshot != "on" && o.SystemPromptSnapshot != "off":
		return agent.Invalid("provider.system_prompt_snapshot must be on or off")
	case o.FileCheckpointing != "" && o.FileCheckpointing != "on" && o.FileCheckpointing != "off":
		return agent.Invalid("provider.file_checkpointing must be on or off")
	case req.Stateless() && o.FileCheckpointing == "on":
		return agent.Invalid("provider.file_checkpointing needs a workspace")
	case o.MaxTurns < 0 || o.MaxTurns > 1000:
		return agent.Invalid("provider.max_turns must be between 1 and 1000")
	case o.MaxOutputTokens != 0 && (o.MaxOutputTokens < 256 || o.MaxOutputTokens > 128_000):
		return agent.Invalid("provider.max_output_tokens must be between 256 and 128000")
	case o.DebugFilter != "" && !o.Debug:
		return agent.Invalid("provider.debug_filter requires provider.debug true")
	case strings.ContainsAny(o.DebugFilter, " \t\r\n\x00") || len(o.DebugFilter) > 256:
		return agent.Invalid("provider.debug_filter must be at most 256 non-whitespace characters")
	}

	return nil
}

// modelPattern mirrors the request model check for option values naming models.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func validModelList(value string) bool {
	models := strings.Split(value, ",")
	if len(models) > maxOptionList {
		return false
	}
	for _, model := range models {
		if !modelPattern.MatchString(strings.TrimSpace(model)) {
			return false
		}
	}

	return true
}

func validAutocompact(value string) bool {
	if value == "auto" {
		return true
	}
	if !autocompactRule.MatchString(value) {
		return false
	}
	lower := strings.ToLower(value)
	multiplier := float64(1)
	number := lower
	switch lower[len(lower)-1] {
	case 'k':
		multiplier, number = 1_000, lower[:len(lower)-1]
	case 'm':
		multiplier, number = 1_000_000, lower[:len(lower)-1]
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return false
	}
	if multiplier == 1 && !strings.Contains(number, ".") && parsed >= 100 && parsed <= 1_000 {
		multiplier = 1_000 // Claude accepts 100..1000 as shorthand for thousands.
	}
	tokens := parsed * multiplier

	return tokens >= 100_000 && tokens <= 1_000_000
}

// flags returns the CLI arguments for the options that map to plain flags.
func (o *RequestOptions) flags() []string {
	var args []string
	add := func(enabled bool, flag string, values ...string) {
		if enabled {
			args = append(args, flag)
			args = append(args, values...)
		}
	}
	add(o.AppendSystemPrompt != "", "--append-system-prompt", o.AppendSystemPrompt)
	add(o.Agent != "", "--agent", o.Agent)
	add(len(o.AllowedTools) > 0, "--allowed-tools", o.AllowedTools...)
	add(len(o.DisallowedTools) > 0, "--disallowed-tools", o.DisallowedTools...)
	add(o.MaxBudgetUSD > 0, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	add(o.FallbackModel != "", "--fallback-model", o.FallbackModel)
	add(o.SessionName != "", "--name", o.SessionName)
	add(o.IncludeHookEvents, "--include-hook-events")
	add(o.ForwardSubagentText, "--forward-subagent-text")
	add(o.Autocompact != "", "--autocompact", o.Autocompact)
	add(o.DisableSlashCommands, "--disable-slash-commands")
	add(o.Bare, "--bare")
	add(o.SafeMode, "--safe-mode")
	add(o.ExcludeDynamicSystemPromptSections, "--exclude-dynamic-system-prompt-sections")
	add(o.PromptSuggestions, "--prompt-suggestions")
	add(o.Brief, "--brief")
	add(len(o.Betas) > 0, "--betas", o.Betas...)
	add(o.AllowDangerouslySkipPermissions, "--allow-dangerously-skip-permissions")
	add(o.MaxTurns > 0, "--max-turns", strconv.Itoa(o.MaxTurns))
	if o.Debug {
		if o.DebugFilter == "" {
			args = append(args, "--debug")
		} else {
			args = append(args, "--debug="+o.DebugFilter)
		}
	}
	add(o.SystemPromptSnapshot != "", "--system-prompt-snapshot", o.SystemPromptSnapshot)
	add(len(o.Files) > 0, "--file", o.Files...)
	if len(o.Agents) > 0 {
		encoded, _ := json.Marshal(o.Agents)
		args = append(args, "--agents", string(encoded))
	}

	return args
}

// settings returns the --settings JSON: the client's settings with the gateway policy
// applied on top (file tools stay inside the working directory; no silent model fallback
// unless options.fallback_model asks for it).
// protectedConfig are the files Claude Code itself executes or trusts: MCP servers, hooks,
// settings, agents, skills, commands, and git hooks. Under the CLI sandbox the Bash tool cannot
// write them, but the file tools (Write, Edit) run outside that sandbox, so a prompt-injected
// agent could plant an MCP server that the next run starts unconfined (docs/security.md F-7).
// Existing files keep working; only writes by the agent are denied.
var protectedConfig = []string{".mcp.json", ".claude/**", ".git/**"}

// configDenyRules returns Edit deny rules (Edit covers every file-editing tool) for the
// working directory and each extra directory.
func configDenyRules(addDirs []string) []any {
	rules := make([]any, 0, len(protectedConfig)*(1+len(addDirs)))
	for _, pattern := range protectedConfig {
		rules = append(rules, "Edit(./"+pattern+")")
		for _, dir := range addDirs {
			rules = append(rules, "Edit(/"+strings.TrimSuffix(dir, "/")+"/"+pattern+")") // "//" = absolute.
		}
	}

	return rules
}

func (o *RequestOptions) settings(sandbox agent.SandboxMode, addDirs []string) string {
	merged := maps.Clone(o.Settings)
	if merged == nil {
		merged = map[string]any{}
	}
	permissions, _ := merged["permissions"].(map[string]any)
	permissions = maps.Clone(permissions)
	if permissions == nil {
		permissions = map[string]any{}
	}
	permissions["blockReadsOutsideWorkingDirectories"] = true
	permissions["blockWritesOutsideWorkingDirectories"] = true
	if sandbox == agent.SandboxCLI {
		deny, _ := permissions["deny"].([]any)
		permissions["deny"] = append(slices.Clone(deny), configDenyRules(addDirs)...)
	}
	merged["permissions"] = permissions
	// The sandbox boundary is gateway policy: clients may tune other sandbox keys, but not
	// whether it is enabled or allowed to degrade to unconfined execution.
	sandboxSettings, _ := merged["sandbox"].(map[string]any)
	sandboxSettings = maps.Clone(sandboxSettings)
	if sandboxSettings == nil {
		sandboxSettings = map[string]any{}
	}
	sandboxSettings["enabled"] = sandbox == agent.SandboxCLI
	if sandbox == agent.SandboxCLI {
		sandboxSettings["failIfUnavailable"] = true
	}
	merged["sandbox"] = sandboxSettings
	if o.FallbackModel == "" {
		merged["fallbackModel"] = []any{}
	} else {
		delete(merged, "fallbackModel")
	}
	encoded, _ := json.Marshal(merged)

	return string(encoded)
}
