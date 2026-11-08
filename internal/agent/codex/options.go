package codex

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Limits for native option values.
const (
	maxOptionBytes = 256_000
	maxOptionText  = 256_000
	maxOptionList  = 128
)

var (
	ReasoningSummaries = []string{"auto", "concise", "detailed", "none"}
	ApprovalsReviewers = []string{"user", "auto_review"}

	optionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$`)
)

// gatewayConfigKeys are Codex config keys owned by gateway policy or by explicit request
// fields. A client override of any of them, including a dotted sub-key, is rejected.
var gatewayConfigKeys = []string{
	"approval_policy", "approvals_reviewer", "sandbox_mode", "sandbox_workspace_write",
	"permissions", "default_permissions", "profile", "profiles", "projects",
	"model", "model_provider", "model_providers", "model_reasoning_effort", "review_model",
	"instructions", "developer_instructions", "experimental_instructions_file",
	"forced_login_method", "forced_chatgpt_workspace_id", "cli_auth_credentials_store",
	"chatgpt_base_url", "openai_base_url",
}

// RequestOptions are Codex app-server's native per-run options (the request "options"
// object). See docs/agents/codex-cli.md.
type RequestOptions struct {
	// Config holds native config overrides (mcp_servers, features, tools, web_search, ...).
	Config                map[string]any `json:"config,omitempty"`
	DeveloperInstructions string         `json:"developer_instructions,omitempty"`
	ApprovalsReviewer     string         `json:"approvals_reviewer,omitempty"`
	ServiceTier           string         `json:"service_tier,omitempty"`
	ReasoningSummary      string         `json:"reasoning_summary,omitempty"`
	// DisabledPluginIDs replaces the thread's disabled plugins; an empty list clears it.
	DisabledPluginIDs *[]string `json:"disabled_plugin_ids,omitempty"`
	// SkillRoots add skill directories inside an allowed root or the workspace. They live in
	// the run's own app-server process and are never written to the Codex config.
	SkillRoots []string `json:"skill_roots,omitempty"`
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

	err := o.validate(req)

	return o, err
}

func (o *RequestOptions) validate(req *agent.Request) error {
	if req.Stateless() && (len(o.Config) > 0 || o.ApprovalsReviewer != "" || o.DisabledPluginIDs != nil || len(o.SkillRoots) > 0) {
		return agent.Invalid("without a workspace only developer_instructions, service_tier and reasoning_summary are allowed")
	}
	for key := range o.Config {
		root, _, _ := strings.Cut(key, ".")
		if slices.Contains(gatewayConfigKeys, root) {
			return agent.Invalid("provider.config.%s is controlled by the gateway or an explicit request field", key)
		}
	}
	if o.DisabledPluginIDs != nil {
		if len(*o.DisabledPluginIDs) > maxOptionList {
			return agent.Invalid("provider.disabled_plugin_ids allows at most %d entries", maxOptionList)
		}
		for _, id := range *o.DisabledPluginIDs {
			if !optionNamePattern.MatchString(id) {
				return agent.Invalid("provider.disabled_plugin_ids contains an invalid id %q", id)
			}
		}
	}
	if len(o.SkillRoots) > agent.MaxAddDirs {
		return agent.Invalid("provider.skill_roots allows at most %d entries", agent.MaxAddDirs)
	}
	for i, dir := range o.SkillRoots {
		resolved, err := req.Dir("options.skill_roots", dir)
		if err != nil {
			return err
		}
		o.SkillRoots[i] = resolved
	}
	switch {
	case len(o.DeveloperInstructions) > maxOptionText:
		return agent.Invalid("provider.developer_instructions exceeds %d bytes", maxOptionText)
	case o.ApprovalsReviewer != "" && req.Permissions != agent.PermissionsAsk:
		return agent.Invalid("provider.approvals_reviewer needs permissions ask (bypass never asks)")
	case o.ApprovalsReviewer != "" && !slices.Contains(ApprovalsReviewers, o.ApprovalsReviewer):
		return agent.Invalid("provider.approvals_reviewer must be one of %v", ApprovalsReviewers)
	case o.ServiceTier != "" && !optionNamePattern.MatchString(o.ServiceTier):
		return agent.Invalid("provider.service_tier is not a valid tier name")
	case o.ReasoningSummary != "" && !slices.Contains(ReasoningSummaries, o.ReasoningSummary):
		return agent.Invalid("provider.reasoning_summary must be one of %v", ReasoningSummaries)
	}

	return nil
}

// threadParams adds thread-level options to thread/start, thread/resume and thread/fork.
func (o *RequestOptions) threadParams(params map[string]any) {
	if len(o.Config) > 0 {
		params["config"] = o.Config
	}
	if params["ephemeral"] == true {
		// A call without a workspace loads no MCP server: their tools run outside the sandbox.
		params["config"] = map[string]any{"mcp_servers": map[string]any{}}
	}
	if o.DeveloperInstructions != "" {
		params["developerInstructions"] = o.DeveloperInstructions
	}
	if o.ApprovalsReviewer != "" {
		params["approvalsReviewer"] = o.ApprovalsReviewer
	}
	if o.ServiceTier != "" {
		params["serviceTier"] = o.ServiceTier
	}
}

// turnParams adds turn-level options to turn/start.
func (o *RequestOptions) turnParams(params map[string]any) {
	if o.ReasoningSummary != "" {
		params["summary"] = o.ReasoningSummary
	}
	if o.DisabledPluginIDs != nil {
		params["disabledPluginIds"] = *o.DisabledPluginIDs
	}
}
