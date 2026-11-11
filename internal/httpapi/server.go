// Package httpapi exposes the gateway over HTTP.
//
// Operations are registered with Huma (net/http ServeMux underneath); the OpenAPI 3.1
// document is generated from their Go types, served at /openapi.yaml and rendered at /docs.
// Routes are versioned under /v1. Every route except probes and the docs requires the
// bearer token when one is configured. Errors are RFC 9457 problem details with a stable code
// and the request ID (X-Request-ID, accepted from the client or created). Successful
// responses carry the resource itself; lists are {items, next_cursor}.
//
// Streaming: a run's event log can be read as Server-Sent Events (GET /v1/runs/{id}/events,
// resumable with Last-Event-ID) or over a WebSocket (GET /v1/runs/{id}/ws) that also accepts
// control messages. Both are readers of the same log (see package run).
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/fsroot"
	"github.com/emreuyguc/native-agent-api/internal/permission"
	"github.com/emreuyguc/native-agent-api/internal/run"
	"github.com/emreuyguc/native-agent-api/internal/session"
	"github.com/emreuyguc/native-agent-api/internal/workspace"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	base       context.Context // Server lifetime; bounds background work such as logins.
	logins     logins
	models     modelCatalog
	cfg        config.Config
	log        *slog.Logger
	agent      agent.Agent
	runs       *run.Manager
	rules      *permission.Store
	workspaces *workspace.Store
	roots      *fsroot.Set
	sessions   *session.Registry
	slots      chan struct{}
	version    struct {
		mu    sync.Mutex
		value string
	}
}

// New returns a Server for one agent. Cancelling base cancels every running run (used on
// shutdown).
func New(base context.Context, cfg config.Config, a agent.Agent, logger *slog.Logger) (*Server, error) {
	roots, err := fsroot.New(cfg.AllowedRoots)
	if err != nil {
		return nil, fmt.Errorf("%sALLOWED_ROOTS: %w", config.EnvPrefix, err)
	}
	store, err := workspace.NewStore(cfg.WorkspaceRoot, roots, cfg.Limits.MaxFileBytes, cfg.Limits.MaxListEntries)
	if err != nil {
		return nil, err
	}
	if reporter, ok := a.(agent.ProfileReporter); ok {
		store.Protect(reporter.ProfileDir())
	}
	sessions, err := session.NewRegistry(filepath.Join(cfg.WorkspaceRoot, ".sessions"), func(workspaceID string) bool {
		_, err := store.Dir(workspaceID)
		return err == nil
	})
	if err != nil {
		return nil, err
	}

	return &Server{
		base:       base,
		cfg:        cfg,
		log:        logger,
		agent:      a,
		runs:       run.NewManager(base, cfg.Limits.RunRetention, cfg.Limits.MaxRetainedRuns, logger),
		rules:      permission.NewStore(),
		workspaces: store,
		sessions:   sessions,
		roots:      roots,
		slots:      make(chan struct{}, cfg.Limits.MaxConcurrent),
	}, nil
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	api := s.newAPI(mux)
	// Unknown paths answer with the same problem format as every other error.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "No operation exists at this path.")
	})
	public := map[string]bool{healthPath: true, readyPath: true}
	if s.cfg.Docs {
		// Public on purpose: browsers cannot attach a bearer token to page loads. The spec holds
		// no secrets; the allowlist still applies and every API call still needs the token.
		spec, err := api.OpenAPI().YAML()
		if err != nil {
			panic(fmt.Sprintf("render OpenAPI document: %v", err)) // Static input: a bug, not a runtime condition.
		}
		specJSON, err := api.OpenAPI().MarshalJSON()
		if err != nil {
			panic(fmt.Sprintf("render OpenAPI document: %v", err))
		}
		mux.HandleFunc("GET /openapi.yaml", serveSpec("application/yaml", spec))
		mux.HandleFunc("GET /openapi.json", serveSpec("application/json", specJSON))
		mux.HandleFunc("GET /docs", s.docs)
		mux.HandleFunc("GET /docs/init.js", s.docsScript)
		for _, path := range []string{"/openapi.yaml", "/openapi.json", "/docs", "/docs/init.js"} {
			public[path] = true
		}
	}

	return requestID(s.recoverPanics(s.logRequests(s.allowlist(s.browserGuard(securityHeaders(s.authenticate(mux, public)))))))
}

// Wait blocks until every run has settled after the server context is cancelled.
func (s *Server) Wait() { s.runs.Wait() }
