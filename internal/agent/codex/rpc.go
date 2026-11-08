package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

const modelPageSize = 1_000

// Models implements agent.ModelReporter using Codex's native model catalog.
func (a *Agent) Models(ctx context.Context) ([]agent.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	var models []agent.Model
	var cursor *string
	for {
		result, err := a.rpc(ctx, "model/list", map[string]any{
			"cursor": cursor, "limit": modelPageSize, "includeHidden": false,
		})
		if err != nil {
			return nil, err
		}
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, errors.Join(agent.ErrUpstream, fmt.Errorf("decode Codex model catalog: %w", err))
		}
		for _, raw := range page.Data {
			var native struct {
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			}
			if err := json.Unmarshal(raw, &native); err != nil || native.Model == "" {
				return nil, errors.Join(agent.ErrUpstream, errors.New("codex model catalog contains an invalid model"))
			}
			efforts := []string{}
			for _, option := range native.SupportedReasoningEfforts {
				efforts = append(efforts, option.ReasoningEffort)
			}
			name := native.DisplayName
			if name == "" {
				name = native.Model
			}
			models = append(models, agent.Model{
				ID: native.Model, Name: name, Efforts: efforts,
				Provider: agent.ModelProvider{Provider: agent.NewProvider(Name, nil, raw)},
			})
		}
		if page.NextCursor == nil {
			return models, nil
		}
		cursor = page.NextCursor
	}
}

// Usage implements agent.UsageReporter. Account allowance and token activity remain separate
// native documents because Codex assigns them different scopes.
func (a *Agent) Usage(ctx context.Context) (agent.Usage, error) {
	rateLimits, err := a.rpc(ctx, "account/rateLimits/read", map[string]bool{
		"excludeResetCreditDetails": false, "supportsLunaReserve": false,
	})
	if err != nil {
		return agent.Usage{}, err
	}
	tokenUsage, err := a.rpc(ctx, "account/usage/read", map[string]any{"threadId": nil})
	if err != nil {
		return agent.Usage{}, err
	}
	native, err := json.Marshal(map[string]json.RawMessage{"rate_limits": rateLimits, "token_usage": tokenUsage})
	if err != nil {
		return agent.Usage{}, fmt.Errorf("encode Codex usage: %w", err)
	}
	windows, minutes, err := decodeRateLimits(rateLimits)
	if err != nil {
		return agent.Usage{}, err
	}
	var fields SubscriptionFields
	if len(minutes) > 0 {
		fields.WindowMinutes = minutes
	}

	return agent.Usage{Windows: windows, Provider: agent.SubscriptionProvider{Provider: agent.NewProvider(Name, fields, native)}}, nil
}

type rateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	ResetsAt           *int64  `json:"resetsAt"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
}

type rateLimitSnapshot struct {
	LimitID   *string          `json:"limitId"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
}

// decodeRateLimits flattens the multi-bucket view; the single-bucket view is the same data
// for CLIs that report only one bucket.
func decodeRateLimits(document json.RawMessage) ([]agent.UsageWindow, map[string]int64, error) {
	var native struct {
		RateLimits          *rateLimitSnapshot           `json:"rateLimits"`
		RateLimitsByLimitID map[string]rateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(document, &native); err != nil {
		return nil, nil, errors.Join(agent.ErrUpstream, errors.New("codex returned an invalid rate-limit document"))
	}
	buckets := native.RateLimitsByLimitID
	if len(buckets) == 0 && native.RateLimits != nil {
		id := "rateLimits" // The native field name when the CLI reports no limit identifier.
		if native.RateLimits.LimitID != nil {
			id = *native.RateLimits.LimitID
		}
		buckets = map[string]rateLimitSnapshot{id: *native.RateLimits}
	}
	windows, minutes := []agent.UsageWindow{}, map[string]int64{}
	for _, id := range slices.Sorted(maps.Keys(buckets)) {
		for name, window := range map[string]*rateLimitWindow{"primary": buckets[id].Primary, "secondary": buckets[id].Secondary} {
			if window == nil {
				continue
			}
			entry := agent.UsageWindow{ID: id + "." + name, UsedPercent: window.UsedPercent}
			if window.WindowDurationMins != nil {
				minutes[entry.ID] = *window.WindowDurationMins
			}
			if window.ResetsAt != nil {
				resets := time.Unix(*window.ResetsAt, 0).UTC()
				entry.ResetsAt = &resets
			}
			windows = append(windows, entry)
		}
	}
	slices.SortStableFunc(windows, func(a, b agent.UsageWindow) int { return strings.Compare(a.ID, b.ID) })

	return windows, minutes, nil
}

func (a *Agent) rpc(parent context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, a.opts.CommandTimeout)
	defer cancel()
	cmd := commandProcess(ctx, a.opts.Binary, "app-server", "--listen", "stdio://")
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stderr := iox.NewTailBuffer(16_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		return nil, fmt.Errorf("open Codex app-server maintenance pipes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server maintenance command: %w", err)
	}
	transport := boundedRPC{scanner: bufio.NewScanner(stdout), writer: stdin, limit: a.opts.MaxOutputBytes}
	transport.scanner.Buffer(make([]byte, 64<<10), int(a.opts.MaxOutputBytes))
	if _, err := transport.call(1, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "native_agent_api", "title": "Native Agent API", "version": "0",
	}}); err != nil {
		return nil, finishRPC(cmd, stdin, stderr, err)
	}
	if err := transport.write(map[string]any{"method": "initialized"}); err != nil {
		return nil, finishRPC(cmd, stdin, stderr, err)
	}
	result, callErr := transport.call(2, method, params)
	if err := finishRPC(cmd, stdin, stderr, callErr); err != nil {
		return nil, err
	}

	return result, nil
}

type boundedRPC struct {
	scanner  *bufio.Scanner
	writer   io.Writer
	consumed int64
	limit    int64
}

func (t *boundedRPC) call(id int, method string, params any) (json.RawMessage, error) {
	if err := t.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for t.scanner.Scan() {
		line := append([]byte(nil), t.scanner.Bytes()...)
		t.consumed += int64(len(line))
		if t.consumed > t.limit {
			return nil, errOutputLimit
		}
		var message rpcMessage
		if !json.Valid(line) || json.Unmarshal(line, &message) != nil {
			return nil, errInvalidStream
		}
		if idKey(message.ID) != strconv.Itoa(id) || message.Method != "" {
			continue
		}
		if message.Error != nil {
			return nil, errors.Join(agent.ErrUpstream, fmt.Errorf("codex RPC %d: %s", message.Error.Code, redact.Message(message.Error.Message, maxErrorMessage)))
		}

		return message.Result, nil
	}
	if err := t.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Codex app-server maintenance response: %w", err)
	}

	return nil, errProcessEnded
}

func (t *boundedRPC) write(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = t.writer.Write(append(encoded, '\n'))

	return err
}

func finishRPC(cmd *exec.Cmd, stdin io.Closer, stderr *iox.TailBuffer, prior error) error {
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if prior != nil {
		return prior
	}
	if waitErr == nil {
		return nil
	}
	message := redact.Message(stderr.String(), maxErrorMessage)
	if message == "" {
		message = "Codex app-server maintenance command failed"
	}

	return errors.Join(agent.ErrUpstream, errors.New(message))
}
