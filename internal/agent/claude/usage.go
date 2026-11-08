package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// usageURL is an undocumented Claude application endpoint; it can change without notice.
const usageURL = "https://api.anthropic.com/api/oauth/usage"

const maxUsageBody = 1_000_000

// errUsageScope explains the most common cause of a 403 from the usage endpoint.
var errUsageScope = fmt.Errorf("%w: subscription usage requires a normal `claude auth login`, not setup-token", agent.ErrForbidden)

// Usage implements agent.UsageReporter with the subscription usage document.
func (a *Agent) Usage(ctx context.Context) (agent.Usage, error) {
	body, err := a.usageDocument(ctx)
	if err != nil {
		return agent.Usage{}, err
	}

	return decodeUsage(body)
}

// decodeUsage reads the native `limits` list. Other native fields are left in Native.
func decodeUsage(body []byte) (agent.Usage, error) {
	var native struct {
		Limits []struct {
			Kind     string     `json:"kind"`
			Percent  *float64   `json:"percent"`
			ResetsAt *time.Time `json:"resets_at"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &native); err != nil {
		return agent.Usage{}, errors.Join(agent.ErrUpstream, errors.New("claude returned an invalid usage document"))
	}
	usage := agent.Usage{Windows: []agent.UsageWindow{}, Provider: agent.SubscriptionProvider{Provider: agent.NewProvider(Name, nil, body)}}
	for _, limit := range native.Limits {
		if limit.Kind == "" || limit.Percent == nil {
			continue
		}
		usage.Windows = append(usage.Windows, agent.UsageWindow{ID: limit.Kind, UsedPercent: *limit.Percent, ResetsAt: limit.ResetsAt})
	}

	return usage, nil
}

func (a *Agent) usageDocument(ctx context.Context) ([]byte, error) {
	token, err := a.oauthAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-code")

	resp, err := a.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, agent.ErrLoginExpired
	case resp.StatusCode == http.StatusForbidden:
		return nil, errUsageScope
	case err != nil || len(body) > maxUsageBody || resp.StatusCode/100 != 2 || !json.Valid(body):
		return nil, agent.ErrUpstream
	}

	return body, nil
}

// ReadOAuthAccessToken extracts only the subscription OAuth access token from the CLI
// credential file in profileDir. No other credential field is ever read.
func ReadOAuthAccessToken(profileDir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(profileDir, ".credentials.json")) //nolint:gosec // G304: path comes from operator configuration, not from clients.
	if err != nil {
		return "", agent.ErrLoginRequired
	}

	return accessToken(body)
}
