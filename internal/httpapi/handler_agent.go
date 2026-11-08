package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

type healthStatus struct {
	Status string `json:"status" example:"ok"`
}

func (s *Server) health(context.Context, *struct{}) (*body[healthStatus], error) {
	return reply(healthStatus{Status: "ok"}), nil
}

type componentStatus struct {
	Status string `json:"status" enum:"healthy,unhealthy,saturated"`
}

type readinessStatus struct {
	Status     string                     `json:"status" enum:"ready,not_ready"`
	Components map[string]componentStatus `json:"components"`
	ActiveRuns int                        `json:"active_runs"`
	RunLimit   int                        `json:"run_limit"`
}

type readinessOutput struct {
	Status int
	Body   readinessStatus
}

func (s *Server) readiness(ctx context.Context, _ *struct{}) (*readinessOutput, error) {
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.CommandTimeout)
	defer cancel()
	components := map[string]componentStatus{
		"adapter": {Status: "healthy"}, "auth": {Status: "healthy"},
		"storage": {Status: "healthy"}, "limits": {Status: "healthy"},
	}
	ready := true
	if _, err := s.agent.Version(probeCtx); err != nil {
		components["adapter"] = componentStatus{Status: "unhealthy"}
		ready = false
	}
	if status, err := s.agent.AuthStatus(probeCtx); err != nil || !status.LoggedIn {
		components["auth"] = componentStatus{Status: "unhealthy"}
		ready = false
	}
	if err := errors.Join(s.workspaces.Health(), s.sessions.Health()); err != nil {
		components["storage"] = componentStatus{Status: "unhealthy"}
		ready = false
	}
	active, limit := len(s.slots), cap(s.slots)
	if active >= limit {
		components["limits"] = componentStatus{Status: "saturated"}
		ready = false
	}
	status, code := "ready", http.StatusOK
	if !ready {
		status, code = "not_ready", http.StatusServiceUnavailable
	}

	return &readinessOutput{Status: code, Body: readinessStatus{
		Status: status, Components: components, ActiveRuns: active, RunLimit: limit,
	}}, nil
}

// agentDescription describes the served agent, grouped by who the data belongs to.
type agentDescription struct {
	Agent        agentIdentity      `json:"agent"`
	CLI          agentCLI           `json:"cli"`
	Profile      agentProfile       `json:"profile"`
	Login        agentLogin         `json:"login"`
	Capabilities []agent.Capability `json:"capabilities" doc:"Native CLI surfaces this adapter implements, sorted. Gateway-wide REST, streaming, file and workspace features are described by this document instead."`
	Gateway      gatewayInfo        `json:"gateway"`
}

type agentIdentity struct {
	Name   string `json:"name" doc:"Stable identifier used in the API and configuration; the name of every provider section." example:"claude"`
	Title  string `json:"title" doc:"Product name of the CLI." example:"Claude Code"`
	Vendor string `json:"vendor" example:"Anthropic"`
}

type agentCLI struct {
	Binary        string `json:"binary" doc:"Executable the adapter starts."`
	Version       string `json:"version,omitempty" doc:"Version reported by the native CLI; absent while the CLI cannot report it (see /ready)." example:"2.1.318 (Claude Code)"`
	TestedVersion string `json:"tested_version" doc:"CLI version the adapter is verified against."`
}

type agentProfile struct {
	Dir string `json:"dir,omitempty" doc:"Effective native profile directory of this process."`
}

type agentLogin struct {
	Methods []string `json:"methods" doc:"Native login methods accepted by POST /v1/agent/login/start."`
}

type gatewayInfo struct {
	Build        buildinfo.Info `json:"build" doc:"Build of this gateway."`
	AllowedRoots []string       `json:"allowed_roots" doc:"Directories that can be attached as workspaces or used as extra directories (NATIVE_AGENT_ALLOWED_ROOTS)."`
}

// agentInfo describes the served agent, its CLI version and this gateway.
func (s *Server) agentInfo(ctx context.Context, _ *struct{}) (*body[agentDescription], error) {
	info := s.agent.Info()
	description := agentDescription{
		Agent:        agentIdentity{Name: info.Name, Title: info.Title, Vendor: info.Provider},
		CLI:          agentCLI{Binary: info.Binary, Version: s.cliVersion(ctx), TestedVersion: info.TestedVersion},
		Login:        agentLogin{Methods: append([]string{}, info.LoginMethods...)},
		Capabilities: append([]agent.Capability{}, info.Capabilities...),
		Gateway:      gatewayInfo{Build: buildinfo.Get(), AllowedRoots: append([]string{}, s.roots.Roots()...)},
	}
	if reporter, ok := s.agent.(agent.ProfileReporter); ok {
		description.Profile.Dir = reporter.ProfileDir()
	}

	return reply(description), nil
}

// cliVersion reads the CLI version once; a failed read is retried on the next call.
func (s *Server) cliVersion(ctx context.Context) string {
	s.version.mu.Lock()
	defer s.version.mu.Unlock()
	if s.version.value == "" {
		output, err := s.agent.Version(ctx)
		if err != nil {
			s.log.WarnContext(ctx, "agent version command failed", "err", err)
			return ""
		}
		s.version.value = strings.TrimSpace(output)
	}

	return s.version.value
}

func (s *Server) agentDiagnostics(ctx context.Context, _ *struct{}) (*body[agent.Diagnostics], error) {
	reporter, ok := s.agent.(agent.DiagnosticsReporter)
	if !ok {
		return nil, agentError(agent.ErrUnsupported)
	}
	diagnostics, err := reporter.Diagnostics(ctx)
	if err != nil {
		return nil, agentError(err)
	}
	diagnostics.Output = strings.TrimSpace(diagnostics.Output)

	return reply(diagnostics), nil
}

func (s *Server) agentAuthStatus(ctx context.Context, _ *struct{}) (*body[agent.AuthStatus], error) {
	status, err := s.agent.AuthStatus(ctx)
	if err != nil {
		return nil, agentError(err)
	}

	return reply(status), nil
}

func (s *Server) agentUsage(ctx context.Context, _ *struct{}) (*body[agent.Usage], error) {
	reporter, ok := s.agent.(agent.UsageReporter)
	if !ok {
		return nil, agentError(agent.ErrUnsupported)
	}
	usage, err := reporter.Usage(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "agent usage request failed", "err", err)
		return nil, agentError(err)
	}

	return reply(usage), nil
}

// agentModels reports the native catalog and refreshes the catalog used for effort validation.
func (s *Server) agentModels(ctx context.Context, _ *struct{}) (*body[list[agent.Model]], error) {
	models, err := s.models.refresh(ctx, s.agent)
	if err != nil {
		return nil, agentError(err)
	}

	return reply(list[agent.Model]{Items: models}), nil
}

// agentError maps agent sentinel errors to API errors.
func agentError(err error) error {
	status, code, message := agentErrorInfo(err)
	return fail(status, code, message)
}

func agentErrorInfo(err error) (status int, code, message string) {
	var mismatch *agent.ProviderMismatchError
	switch {
	case errors.As(err, &mismatch):
		return http.StatusUnprocessableEntity, "provider_mismatch", err.Error()
	case errors.Is(err, agent.ErrInvalidRequest):
		return http.StatusUnprocessableEntity, "invalid_request", strings.TrimPrefix(err.Error(), agent.ErrInvalidRequest.Error()+": ")
	case errors.Is(err, agent.ErrLoginRequired):
		return http.StatusConflict, "login_required", err.Error()
	case errors.Is(err, agent.ErrLoginExpired):
		return http.StatusUnauthorized, "login_expired", err.Error()
	case errors.Is(err, agent.ErrForbidden):
		return http.StatusForbidden, "forbidden", err.Error()
	case errors.Is(err, agent.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported", err.Error()
	default:
		return http.StatusBadGateway, "agent_failed", err.Error()
	}
}
