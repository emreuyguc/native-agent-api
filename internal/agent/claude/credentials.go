package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// keychainService is the macOS Keychain service Claude Code stores its login under.
const keychainService = "Claude Code-credentials"

// securityTool is the macOS Keychain command-line tool.
const securityTool = "/usr/bin/security"

// KeychainService returns the Keychain service name the pinned Claude CLI uses for a profile.
// The native profile uses the plain name; a custom CLAUDE_CONFIG_DIR adds the first eight hex
// digits of the SHA-256 of the NFC-normalized directory. This mirrors the pinned CLI and is
// re-verified whenever the pinned version changes.
func KeychainService(dir string, custom bool) string {
	if !custom {
		return keychainService
	}
	sum := sha256.Sum256([]byte(norm.NFC.String(dir)))

	return keychainService + "-" + hex.EncodeToString(sum[:])[:8]
}

// oauthAccessToken returns only the subscription OAuth access token of the configured
// profile: from the macOS Keychain when present, otherwise from the CLI credential file.
func (a *Agent) oauthAccessToken(ctx context.Context) (string, error) {
	dir, custom, err := a.opts.profile()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		if token, err := a.keychainAccessToken(ctx, KeychainService(dir, custom)); err == nil {
			return token, nil
		}
	}

	return ReadOAuthAccessToken(dir)
}

func (a *Agent) keychainAccessToken(ctx context.Context, service string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityTool, "find-generic-password", "-s", service, "-a", a.opts.User, "-w") //nolint:gosec // G204: the fixed macOS security tool; arguments are separate, no shell.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + a.opts.HomeDir, "USER=" + a.opts.User}
	output, err := cmd.Output()
	if err != nil {
		return "", agent.ErrLoginRequired
	}

	return accessToken(output)
}

// accessToken extracts claudeAiOauth.accessToken; no other credential field is kept.
func accessToken(document []byte) (string, error) {
	var credentials struct {
		ClaudeAI struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(document, &credentials); err != nil || !strings.HasPrefix(credentials.ClaudeAI.AccessToken, "sk-ant-") {
		return "", agent.ErrLoginRequired
	}

	return credentials.ClaudeAI.AccessToken, nil
}
