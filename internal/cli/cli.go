// Package cli implements the native-agent-api command line.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"
	"github.com/emreuyguc/native-agent-api/internal/logx"
)

const usage = `Usage: ` + buildinfo.Name + ` <command>

Commands:
  serve [--env-file <path>]
                  Start the HTTP API using process environment and an optional explicit env file
  login [agent]   Run the agent's native login flow
  status [agent]  Show the agent's native login status
  new-token       Generate an API token and its SHA-256 digest
  openapi         Print the generated OpenAPI 3.1 document (YAML)
  version         Print the gateway version
  healthcheck     Exit 0 when the local /health endpoint answers
  help            Show this help

One gateway process serves one explicitly configured agent (NATIVE_AGENT_AGENT); the
optional [agent] argument overrides it for login/status.

Configuration is read once from NATIVE_AGENT_* environment variables at startup and
cannot be changed while the gateway runs.
`

// Env abstracts the process environment so Run is testable.
type Env struct {
	Args []string
	// Environ is the process environment as a map (see EnvironMap).
	Environ map[string]string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run executes the command in env.Args (without the program name) and returns an exit code.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)

		return 2
	}
	command := env.Args[0]

	var err error
	switch command {
	case "serve":
		err = serve(ctx, env)
	case "login":
		err = interactive(ctx, env, agent.CommandLogin)
	case "status":
		err = interactive(ctx, env, agent.CommandStatus)
	case "new-token":
		err = newToken(env)
	case "openapi":
		if err = buildinfo.Validate(); err == nil {
			var spec []byte
			if spec, err = httpapi.OpenAPI(); err == nil {
				_, err = env.Stdout.Write(spec)
			}
		}
	case "version", "--version", "-v":
		if err = buildinfo.Validate(); err == nil {
			_, err = fmt.Fprintln(env.Stdout, buildinfo.Get())
		}
	case "healthcheck":
		err = healthcheck(ctx, env)
	case "help", "--help", "-h":
		_, err = fmt.Fprint(env.Stdout, usage)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", command, usage)

		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(env.Stderr, "error:", err)

		return 1
	}

	return 0
}

func serve(ctx context.Context, env Env) error {
	if err := buildinfo.Validate(); err != nil {
		return err
	}
	environ := env.Environ
	switch {
	case len(env.Args) == 1:
	case len(env.Args) == 3 && env.Args[1] == "--env-file":
		var err error
		environ, err = config.MergeFile(env.Environ, env.Args[2])
		if err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	default:
		return errors.New("serve usage: native-agent-api serve [--env-file <path>]")
	}
	cfg, err := config.Load(environ)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err) // No logger yet: printed by Run.
	}
	logger := logx.New(env.Stderr, cfg.Log)
	fail := func(msg string, err error) error {
		logx.Critical(ctx, logger, msg, "err", err)
		return err
	}

	if err := config.PrepareDataDir(cfg.WorkDir); err != nil {
		return fail("data directory setup failed", err)
	}
	served, err := builtin.Build(cfg, logger)
	if err != nil {
		return fail("agent setup failed", err)
	}
	api, err := httpapi.New(ctx, cfg, served, logger)
	if err != nil {
		return fail("server setup failed", err)
	}
	warnInsecureSettings(logger, cfg)

	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.Network.Listen)
	if err != nil {
		return fail("cannot listen", err)
	}
	server := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Minute, // Large uploads stream through ReadTimeout.
		WriteTimeout:      cfg.Limits.QueryTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	logger.Info("gateway started",
		"address", listener.Addr().String(),
		"version", buildinfo.Get().Version,
		"agent", served.Name(),
		"auth", cfg.Auth.Enabled,
		"allowed_ips", len(cfg.Network.AllowedIPs),
		"log_requests", cfg.Log.Requests,
		"log_bodies", cfg.Log.Bodies,
	)

	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()

	select {
	case err := <-errs:
		return fail("server stopped unexpectedly", err)
	case <-ctx.Done():
		logger.Info("shutdown requested; waiting for running requests", "timeout", "10s")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown timed out; closing connections", "err", err)
			return fmt.Errorf("shutdown: %w", err)
		}
		api.Wait()
		if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
			return fail("server stopped unexpectedly", err)
		}
		logger.Info("gateway stopped")

		return nil
	}
}

func interactive(ctx context.Context, env Env, command agent.InteractiveCommand) error {
	if len(env.Args) > 2 {
		return fmt.Errorf("%s usage: native-agent-api %s [agent]", env.Args[0], env.Args[0])
	}
	environ := maps.Clone(env.Environ)
	if len(env.Args) > 1 {
		environ[config.EnvPrefix+"AGENT"] = env.Args[1]
	}
	cfg, err := config.LoadInteractive(environ)
	if err != nil {
		return err
	}
	if err := config.PrepareDataDir(cfg.WorkDir); err != nil {
		return err
	}

	return builtin.Interactive(ctx, cfg, agent.IO{Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr}, command)
}

func healthcheck(ctx context.Context, env Env) error {
	if len(env.Args) != 1 {
		return errors.New("healthcheck usage: native-agent-api healthcheck")
	}
	listen, ok := env.Environ[config.EnvPrefix+"LISTEN"]
	if !ok || listen == "" {
		return fmt.Errorf("%sLISTEN is required", config.EnvPrefix)
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("parse listen address: %w", err)
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/health"
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}

	return nil
}

func warnInsecureSettings(logger *slog.Logger, cfg config.Config) {
	if !cfg.Auth.Enabled {
		logger.Warn("API authentication is disabled: any local process, including the agent started by this gateway, can call the API")
	}
	if cfg.Auth.PlainToken {
		logger.Warn("NATIVE_AGENT_TOKEN is set in clear text and is readable by agent processes running as the same user; prefer NATIVE_AGENT_TOKEN_SHA256",
			"token_command", buildinfo.Name+" new-token")
	}
	if cfg.Log.Bodies {
		logger.Warn("request/response body logging is enabled: prompts and answers are written to the log")
	}
}

// newToken prints a random API token for clients and the digest for the gateway.
func newToken(env Env) error {
	token := rand.Text() + rand.Text()
	digest := config.HashToken(token)
	_, err := fmt.Fprintf(env.Stdout,
		"Client token (Authorization: Bearer …):\n  %s\n\nGateway setting:\n  NATIVE_AGENT_TOKEN_SHA256=%s\n",
		token, hex.EncodeToString(digest[:]))

	return err
}

// OSEnv returns the real process environment.
func OSEnv() Env {
	return Env{
		Args:    os.Args[1:],
		Environ: EnvironMap(os.Environ()),
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
}

// EnvironMap converts os.Environ() style "KEY=value" pairs into a map.
func EnvironMap(pairs []string) map[string]string {
	environ := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if key, value, ok := strings.Cut(pair, "="); ok {
			environ[key] = value
		}
	}

	return environ
}
