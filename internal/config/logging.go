package config

import "log/slog"

// Log configures structured logging.
type Log struct {
	// Level is NATIVE_AGENT_LOG_LEVEL: debug, info, warn or error.
	Level slog.Level
	// Format is NATIVE_AGENT_LOG_FORMAT: json, text or console (colored, for a terminal).
	Format string
	// Requests enables one access log line per request (NATIVE_AGENT_LOG_REQUESTS).
	Requests bool
	// Bodies adds request and response bodies to the access log (NATIVE_AGENT_LOG_BODIES).
	// Bodies are truncated to BodyLimit bytes and credentials are redacted;
	// file transfers are never logged. Prompts and answers will appear in logs.
	Bodies    bool
	BodyLimit int
}
