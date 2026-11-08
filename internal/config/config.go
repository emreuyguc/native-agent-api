// Package config loads and validates the gateway configuration from environment variables.
//
// All gateway variables use the NATIVE_AGENT_ prefix and are parsed with caarlos0/env.
// The configuration is read exactly once at startup and is immutable afterwards: there is no
// reload, no file watching and no API that changes it. Agent processes never inherit the
// gateway environment.
//
// One server process serves exactly one agent selected by NATIVE_AGENT_AGENT.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// EnvPrefix is the prefix shared by every gateway environment variable.
const EnvPrefix = "NATIVE_AGENT_"

// Config is the validated runtime configuration. Treat it as read-only.
type Config struct {
	Network Network
	Auth    Auth
	Log     Log
	// Docs serves /openapi.yaml and the /docs reference UI without a token.
	Docs bool
	// Agent is the single agent served by this process (NATIVE_AGENT_AGENT).
	Agent string
	// SandboxMode is cli (the CLI's own OS sandbox) or external (an outer boundary such as
	// a container isolates the CLI).
	SandboxMode string

	// HomeDir is passed to agent processes as HOME.
	HomeDir string
	// User is passed to agent processes as USER (needed for the macOS Keychain).
	User string
	// SearchPath is passed to agent processes as PATH.
	SearchPath string
	// TempDir hosts per-run private files.
	TempDir string
	// ProxyEnv holds the standard proxy variables that are set, passed on to agent processes.
	ProxyEnv []string
	// DataDir is the gateway's own state directory (NATIVE_AGENT_DATA_DIR).
	DataDir string
	// WorkDir (<DataDir>/work) is the working directory for maintenance commands.
	WorkDir string
	// WorkspaceRoot (<DataDir>/workspaces) holds managed workspaces as <root>/<workspace_id>.
	WorkspaceRoot string
	// AllowedRoots are host directories that may be attached as workspaces or handed to the
	// agent as extra directories (add_dirs, plugin_dirs). Read once at startup; empty
	// disables both.
	AllowedRoots []string
	// ProfileDir is the agent profile (settings, login, history): "native" for the CLI's own
	// default location, or an absolute directory.
	ProfileDir string

	Limits Limits
}

// InteractiveConfig is the complete configuration needed by the login and status commands.
// It intentionally excludes server, authentication, logging, workspace, and run-limit settings.
type InteractiveConfig struct {
	Agent      string
	HomeDir    string
	User       string
	SearchPath string
	TempDir    string
	ProxyEnv   []string
	DataDir    string
	WorkDir    string
	ProfileDir string
}

// proxyVariables are passed on to agent processes when set, so the CLIs and the tools they start
// reach the network through the proxy (the restricted egress overlay, a corporate proxy).
var proxyVariables = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
}

func proxyEnv(environ map[string]string) []string {
	var pairs []string
	for _, name := range proxyVariables {
		if value, ok := environ[name]; ok && value != "" {
			pairs = append(pairs, name+"="+value)
		}
	}

	return pairs
}

// Data directory layout. Both directories are derived from NATIVE_AGENT_DATA_DIR.
const (
	workSubdir       = "work"
	workspacesSubdir = "workspaces"
)

// PrepareDataDir creates the maintenance working directory (mode 0700) and its parents.
func PrepareDataDir(workDir string) error {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("initialize %sDATA_DIR: %w", EnvPrefix, err)
	}

	return nil
}

// ProfileNative selects the native CLI's own default profile location under HOME.
const ProfileNative = "native"

// Limits bounds resource usage per request and per process.
type Limits struct {
	MaxConcurrent  int
	QueryTimeout   time.Duration
	CommandTimeout time.Duration
	// LoginTimeout bounds one API-driven native login flow.
	LoginTimeout time.Duration
	// RunRetention is how long finished runs (and their events) stay readable.
	RunRetention       time.Duration
	MaxRetainedRuns    int
	MaxRunEvents       int
	MaxRunObservations int
	// RunIdle closes an interactive run that has had no turn for this long.
	RunIdle        time.Duration
	MaxBodyBytes   int64
	MaxOutputBytes int64
	MaxFileBytes   int64
	MaxListEntries int
}

// spec is the raw environment schema. Fields ending in ",file" read the named file.
type spec struct {
	Agent       string `env:"AGENT"`
	SandboxMode string `env:"SANDBOX_MODE"`

	Listen               string     `env:"LISTEN"`
	AllowedIPs           []ipPrefix `env:"ALLOWED_IPS" envSeparator:","`
	Token                string     `env:"TOKEN"`
	TokenSHA256          string     `env:"TOKEN_SHA256"`
	TokenSHA256File      string     `env:"TOKEN_SHA256_FILE,file"`
	AllowUnauthenticated bool       `env:"ALLOW_UNAUTHENTICATED"`

	LogLevel     slog.Level `env:"LOG_LEVEL"`
	LogFormat    string     `env:"LOG_FORMAT"`
	LogRequests  bool       `env:"LOG_REQUESTS"`
	LogBodies    bool       `env:"LOG_BODIES"`
	LogBodyLimit int        `env:"LOG_BODY_LIMIT"`

	Docs bool `env:"DOCS"`

	DataDir      string   `env:"DATA_DIR"`
	AllowedRoots []string `env:"ALLOWED_ROOTS" envSeparator:","`
	ProfileDir   string   `env:"PROFILE_DIR"`

	MaxConcurrent         int   `env:"MAX_CONCURRENT"`
	QueryTimeoutSeconds   int   `env:"QUERY_TIMEOUT_SECONDS"`
	CommandTimeoutSeconds int   `env:"COMMAND_TIMEOUT_SECONDS"`
	LoginTimeoutSeconds   int   `env:"LOGIN_TIMEOUT_SECONDS"`
	RunRetentionSeconds   int   `env:"RUN_RETENTION_SECONDS"`
	MaxRetainedRuns       int   `env:"MAX_RETAINED_RUNS"`
	MaxRunEvents          int   `env:"MAX_RUN_EVENTS"`
	MaxRunObservations    int   `env:"MAX_RUN_OBSERVATIONS"`
	RunIdleSeconds        int   `env:"RUN_IDLE_SECONDS"`
	MaxBodyBytes          int64 `env:"MAX_BODY_BYTES"`
	MaxOutputBytes        int64 `env:"MAX_OUTPUT_BYTES"`
	MaxFileBytes          int64 `env:"MAX_FILE_BYTES"`
	MaxListEntries        int   `env:"MAX_LIST_ENTRIES"`
}

// system holds the standard (unprefixed) variables the gateway reads.
type system struct {
	Home    string `env:"HOME"`
	User    string `env:"USER"`
	Path    string `env:"PATH"`
	TempDir string `env:"TMPDIR"`
}

type interactiveSpec struct {
	Agent      string `env:"AGENT"`
	DataDir    string `env:"DATA_DIR"`
	ProfileDir string `env:"PROFILE_DIR"`
}

func validProfileDir(dir string) bool { return dir == ProfileNative || filepath.IsAbs(dir) }

// Load parses and validates the configuration from environ (e.g. the result of
// os.Environ split into a map).
func Load(environ map[string]string) (Config, error) {
	var (
		raw spec
		sys system
	)
	if err := env.ParseWithOptions(&raw, env.Options{
		Environment: environ, Prefix: EnvPrefix, RequiredIfNoDef: true,
	}); err != nil {
		return Config{}, err
	}
	if err := env.ParseWithOptions(&sys, env.Options{
		Environment: environ, RequiredIfNoDef: true,
	}); err != nil {
		return Config{}, err
	}
	agentName := strings.ToLower(strings.TrimSpace(raw.Agent))

	auth, err := newAuth(raw)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Agent:       agentName,
		SandboxMode: strings.ToLower(strings.TrimSpace(raw.SandboxMode)),
		Network:     Network{Listen: raw.Listen, AllowedIPs: prefixes(raw.AllowedIPs)},
		Auth:        auth,
		Docs:        raw.Docs,
		Log: Log{
			Level:     raw.LogLevel,
			Format:    strings.ToLower(raw.LogFormat),
			Requests:  raw.LogRequests,
			Bodies:    raw.LogBodies,
			BodyLimit: raw.LogBodyLimit,
		},
		HomeDir:       sys.Home,
		User:          sys.User,
		SearchPath:    sys.Path,
		TempDir:       sys.TempDir,
		ProxyEnv:      proxyEnv(environ),
		DataDir:       filepath.Clean(raw.DataDir),
		WorkDir:       filepath.Join(raw.DataDir, workSubdir),
		WorkspaceRoot: filepath.Join(raw.DataDir, workspacesSubdir),
		AllowedRoots:  trimAll(raw.AllowedRoots),
		ProfileDir:    strings.TrimSpace(raw.ProfileDir),
		Limits: Limits{
			MaxConcurrent:      raw.MaxConcurrent,
			QueryTimeout:       time.Duration(raw.QueryTimeoutSeconds) * time.Second,
			CommandTimeout:     time.Duration(raw.CommandTimeoutSeconds) * time.Second,
			LoginTimeout:       time.Duration(raw.LoginTimeoutSeconds) * time.Second,
			RunRetention:       time.Duration(raw.RunRetentionSeconds) * time.Second,
			MaxRetainedRuns:    raw.MaxRetainedRuns,
			MaxRunEvents:       raw.MaxRunEvents,
			MaxRunObservations: raw.MaxRunObservations,
			RunIdle:            time.Duration(raw.RunIdleSeconds) * time.Second,
			MaxBodyBytes:       raw.MaxBodyBytes,
			MaxOutputBytes:     raw.MaxOutputBytes,
			MaxFileBytes:       raw.MaxFileBytes,
			MaxListEntries:     raw.MaxListEntries,
		},
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// LoadInteractive parses only the settings used by the login and status commands.
func LoadInteractive(environ map[string]string) (InteractiveConfig, error) {
	var (
		raw interactiveSpec
		sys system
	)
	if err := env.ParseWithOptions(&raw, env.Options{
		Environment: environ, Prefix: EnvPrefix, RequiredIfNoDef: true,
	}); err != nil {
		return InteractiveConfig{}, err
	}
	if err := env.ParseWithOptions(&sys, env.Options{
		Environment: environ, RequiredIfNoDef: true,
	}); err != nil {
		return InteractiveConfig{}, err
	}
	agentName := strings.ToLower(strings.TrimSpace(raw.Agent))

	cfg := InteractiveConfig{
		Agent:      agentName,
		HomeDir:    sys.Home,
		User:       sys.User,
		SearchPath: sys.Path,
		TempDir:    sys.TempDir,
		ProxyEnv:   proxyEnv(environ),
		DataDir:    filepath.Clean(raw.DataDir),
		WorkDir:    filepath.Join(raw.DataDir, workSubdir),
		ProfileDir: strings.TrimSpace(raw.ProfileDir),
	}
	var errs []error
	if cfg.Agent == "" {
		errs = append(errs, fmt.Errorf("%sAGENT must name the agent", EnvPrefix))
	}
	if cfg.User == "" {
		errs = append(errs, errors.New("USER must not be empty"))
	}
	if cfg.SearchPath == "" {
		errs = append(errs, errors.New("PATH must not be empty"))
	}
	for name, dir := range map[string]string{
		"HOME": cfg.HomeDir, "TMPDIR": cfg.TempDir,
	} {
		if !filepath.IsAbs(dir) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path", name))
		}
	}
	if !filepath.IsAbs(raw.DataDir) {
		errs = append(errs, fmt.Errorf("%sDATA_DIR must be an absolute path", EnvPrefix))
	}
	if !validProfileDir(cfg.ProfileDir) {
		errs = append(errs, fmt.Errorf("%sPROFILE_DIR must be %s or an absolute path", EnvPrefix, ProfileNative))
	}
	if err := errors.Join(errs...); err != nil {
		return InteractiveConfig{}, err
	}

	return cfg, nil
}

// Validate checks value ranges and the authentication rule: a non-loopback listener
// requires a token unless NATIVE_AGENT_ALLOW_UNAUTHENTICATED=true is set explicitly.
func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(c.Agent != "", "%sAGENT must name the agent to serve", EnvPrefix)
	check(c.SandboxMode == "cli" || c.SandboxMode == "external", "%sSANDBOX_MODE must be cli or external", EnvPrefix)
	listen, err := netip.ParseAddrPort(c.Network.Listen)
	check(err == nil, "%sLISTEN must be ip:port (e.g. 127.0.0.1:8080)", EnvPrefix)
	if err == nil && !listen.Addr().IsLoopback() {
		check(c.Auth.Enabled || c.Auth.AllowUnauthenticated,
			"%sTOKEN_SHA256 (or TOKEN) is required when listening on a non-loopback address (%s); "+
				"set %sALLOW_UNAUTHENTICATED=true to override", EnvPrefix, c.Network.Listen, EnvPrefix)
	}
	// It may be true on any listener; only a configured token contradicts it.
	check(!c.Auth.AllowUnauthenticated || !c.Auth.Enabled,
		"%sALLOW_UNAUTHENTICATED=true contradicts a configured token: remove the token or set it to false", EnvPrefix)
	check(!c.Log.Bodies || (c.Log.Requests && c.Log.BodyLimit > 0),
		"%sLOG_BODIES=true requires %sLOG_REQUESTS=true and a positive %sLOG_BODY_LIMIT", EnvPrefix, EnvPrefix, EnvPrefix)
	check(c.User != "", "USER must not be empty")
	check(c.SearchPath != "", "PATH must not be empty")
	for name, dir := range map[string]string{
		"HOME":   c.HomeDir,
		"TMPDIR": c.TempDir,
	} {
		check(filepath.IsAbs(dir), "%s must be an absolute path", name)
	}
	check(validProfileDir(c.ProfileDir), "%sPROFILE_DIR must be %s or an absolute path", EnvPrefix, ProfileNative)
	check(filepath.IsAbs(c.DataDir) && filepath.Clean(c.DataDir) != "/", "%sDATA_DIR must be an absolute path other than /", EnvPrefix)
	for _, root := range c.AllowedRoots {
		check(filepath.IsAbs(root), "%sALLOWED_ROOTS entries must be absolute paths, got %q", EnvPrefix, root)
		check(filepath.Clean(root) != "/", "%sALLOWED_ROOTS must not contain /", EnvPrefix)
	}
	check(len(c.AllowedRoots) <= 64, "%sALLOWED_ROOTS allows at most 64 entries", EnvPrefix)
	check(slices.Contains([]string{"json", "text", "console"}, c.Log.Format), "%sLOG_FORMAT must be json, text or console", EnvPrefix)
	check(between(c.Log.BodyLimit, 0, 1<<20), "%sLOG_BODY_LIMIT must be between 0 and 1048576", EnvPrefix)

	l := c.Limits
	check(between(l.MaxConcurrent, 1, 16), "%sMAX_CONCURRENT must be between 1 and 16", EnvPrefix)
	check(between(l.QueryTimeout, 10*time.Second, 30*time.Minute), "%sQUERY_TIMEOUT_SECONDS must be between 10 and 1800", EnvPrefix)
	check(between(l.CommandTimeout, time.Second, 5*time.Minute), "%sCOMMAND_TIMEOUT_SECONDS must be between 1 and 300", EnvPrefix)
	check(between(l.LoginTimeout, time.Minute, time.Hour), "%sLOGIN_TIMEOUT_SECONDS must be between 60 and 3600", EnvPrefix)
	check(between(l.RunRetention, 10*time.Second, 24*time.Hour), "%sRUN_RETENTION_SECONDS must be between 10 and 86400", EnvPrefix)
	check(between(l.MaxRetainedRuns, 1, 10_000), "%sMAX_RETAINED_RUNS must be between 1 and 10000", EnvPrefix)
	check(between(l.MaxRunEvents, 16, 1_000_000), "%sMAX_RUN_EVENTS must be between 16 and 1000000", EnvPrefix)
	check(between(l.MaxRunObservations, 1, 100_000), "%sMAX_RUN_OBSERVATIONS must be between 1 and 100000", EnvPrefix)
	check(between(l.RunIdle, 10*time.Second, time.Hour), "%sRUN_IDLE_SECONDS must be between 10 and 3600", EnvPrefix)
	check(between(l.MaxBodyBytes, 1<<10, 16_000_000), "%sMAX_BODY_BYTES must be between 1024 and 16000000", EnvPrefix)
	check(between(l.MaxOutputBytes, 64<<10, 64_000_000), "%sMAX_OUTPUT_BYTES must be between 65536 and 64000000", EnvPrefix)
	check(between(l.MaxFileBytes, 1<<20, 2<<30), "%sMAX_FILE_BYTES must be between 1048576 and 2147483648", EnvPrefix)
	check(between(l.MaxListEntries, 1, 100_000), "%sMAX_LIST_ENTRIES must be between 1 and 100000", EnvPrefix)

	return errors.Join(errs...)
}

func between[T int | int64 | time.Duration](value, lowest, highest T) bool {
	return value >= lowest && value <= highest
}

func trimAll(values []string) []string {
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}

	return out
}
