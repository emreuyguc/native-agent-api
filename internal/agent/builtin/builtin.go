// Package builtin is the single list of agent adapters compiled into the gateway.
//
// To add an adapter: create internal/agent/<name>/, add its Descriptor to Catalog and a
// constructor case to Build, add docker/<name>/ and docs/agents/<cli-name>.md.
package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/claude"
	"github.com/emreuyguc/native-agent-api/internal/agent/codex"
	"github.com/emreuyguc/native-agent-api/internal/config"
)

// Catalog lists every adapter this binary supports, enabled or not.
func Catalog() []agent.Info {
	return []agent.Info{claude.Descriptor, codex.Descriptor}
}

// Sections maps every adapter to the Go types of its provider sections (the OpenAPI document
// of a binary that is not serving yet describes all of them).
func Sections() map[string]map[agent.Section]reflect.Type {
	return map[string]map[agent.Section]reflect.Type{
		claude.Name: claude.Sections(),
		codex.Name:  codex.Sections(),
	}
}

// Build constructs the adapter named by cfg.Agent.
func Build(cfg config.Config, logger *slog.Logger) (agent.Agent, error) {
	switch cfg.Agent {
	case claude.Name:
		return claude.New(claude.Options{
			ProfileDir:     cfg.ProfileDir,
			HomeDir:        cfg.HomeDir,
			User:           cfg.User,
			WorkDir:        cfg.WorkDir,
			TempDir:        cfg.TempDir,
			ProxyEnv:       cfg.ProxyEnv,
			Path:           cfg.SearchPath,
			SandboxMode:    agent.SandboxMode(cfg.SandboxMode),
			MaxOutputBytes: cfg.Limits.MaxOutputBytes,
			CommandTimeout: cfg.Limits.CommandTimeout,
			Logger:         logger,
		})
	case codex.Name:
		return codex.New(codex.Options{
			ProfileDir:     cfg.ProfileDir,
			HomeDir:        cfg.HomeDir,
			User:           cfg.User,
			WorkDir:        cfg.WorkDir,
			TempDir:        cfg.TempDir,
			ProxyEnv:       cfg.ProxyEnv,
			Path:           cfg.SearchPath,
			SandboxMode:    agent.SandboxMode(cfg.SandboxMode),
			MaxOutputBytes: cfg.Limits.MaxOutputBytes,
			CommandTimeout: cfg.Limits.CommandTimeout,
			Logger:         logger,
		})
	default:
		return nil, fmt.Errorf("%sAGENT: unknown agent %q (supported: %s)", config.EnvPrefix, cfg.Agent, strings.Join(names(), ", "))
	}
}

// Interactive runs a native login or status command without requiring unrelated server settings.
func Interactive(ctx context.Context, cfg config.InteractiveConfig, stdio agent.IO, command agent.InteractiveCommand) error {
	switch cfg.Agent {
	case claude.Name:
		return claude.RunInteractive(ctx, claude.Options{
			Binary:     claude.Binary,
			ProfileDir: cfg.ProfileDir,
			HomeDir:    cfg.HomeDir,
			User:       cfg.User,
			WorkDir:    cfg.WorkDir,
			TempDir:    cfg.TempDir,
			ProxyEnv:   cfg.ProxyEnv,
			Path:       cfg.SearchPath,
		}, stdio, command)
	case codex.Name:
		return codex.RunInteractive(ctx, codex.Options{
			Binary:     codex.Binary,
			ProfileDir: cfg.ProfileDir,
			HomeDir:    cfg.HomeDir,
			User:       cfg.User,
			WorkDir:    cfg.WorkDir,
			TempDir:    cfg.TempDir,
			ProxyEnv:   cfg.ProxyEnv,
			Path:       cfg.SearchPath,
		}, stdio, command)
	default:
		return fmt.Errorf("%sAGENT: unknown agent %q (supported: %s)", config.EnvPrefix, cfg.Agent, strings.Join(names(), ", "))
	}
}

func names() []string {
	catalog := Catalog()
	var out []string
	for i := range catalog {
		out = append(out, catalog[i].Name)
	}

	return out
}
