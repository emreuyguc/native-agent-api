// Package claude runs the native Claude Code CLI in non-interactive stream-json mode.
//
// The gateway never passes arbitrary CLI flags from clients: requests are validated
// (see Request.Normalize) and translated into a fixed argument set (see Arguments).
package claude
