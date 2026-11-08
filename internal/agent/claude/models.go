package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

const (
	modelsRequestID = "native-agent-api-models"
	maxModelsLine   = 4 << 20
)

// modelDiscoveryArguments start a tool-free, hook-free control session. User settings stay
// loaded so native model restrictions such as availableModels still apply.
var modelDiscoveryArguments = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--no-session-persistence",
	"--no-chrome",
	"--tools", "",
	"--strict-mcp-config",
	"--setting-sources", "user",
	"--settings", `{"disableAllHooks":true}`,
}

// Models implements agent.ModelReporter with the model list returned by Claude Code's
// native initialize control request. No prompt is sent, so discovery uses no tokens.
func (a *Agent) Models(ctx context.Context) ([]agent.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.CommandTimeout)
	defer cancel()
	cmd := a.command(ctx, modelDiscoveryArguments...)
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stderr := iox.NewTailBuffer(16_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		return nil, fmt.Errorf("open claude pipes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()
	request := `{"type":"control_request","request_id":"` + modelsRequestID + `","request":{"subtype":"initialize"}}` + "\n"
	if _, err := stdin.Write([]byte(request)); err != nil {
		return nil, fmt.Errorf("send claude initialize: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxModelsLine)
	for scanner.Scan() {
		var message struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  struct {
					Models []json.RawMessage `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil || message.Type != "control_response" ||
			message.Response.RequestID != modelsRequestID {
			continue
		}
		if message.Response.Subtype != "success" {
			return nil, errors.Join(agent.ErrUpstream, fmt.Errorf("claude initialize failed: %s", redact.Message(message.Response.Error, maxErrorMessage)))
		}

		return decodeModels(message.Response.Response.Models)
	}
	message := redact.Message(stderr.String(), maxErrorMessage)
	if message == "" {
		message = "claude exited before reporting models"
	}

	return nil, errors.Join(agent.ErrUpstream, errors.New(message))
}

func decodeModels(raw []json.RawMessage) ([]agent.Model, error) {
	models := make([]agent.Model, 0, len(raw))
	for _, entry := range raw {
		var native struct {
			Value                 string   `json:"value"`
			DisplayName           string   `json:"displayName"`
			SupportsEffort        bool     `json:"supportsEffort"`
			SupportedEffortLevels []string `json:"supportedEffortLevels"`
		}
		if err := json.Unmarshal(entry, &native); err != nil || native.Value == "" {
			return nil, errors.Join(agent.ErrUpstream, errors.New("claude reported an invalid model"))
		}
		efforts := []string{}
		if native.SupportsEffort {
			efforts = append(efforts, native.SupportedEffortLevels...)
		}
		name := native.DisplayName
		if name == "" {
			name = native.Value
		}
		models = append(models, agent.Model{
			ID: native.Value, Name: name, Efforts: efforts,
			Provider: agent.ModelProvider{Provider: agent.NewProvider(Name, nil, entry)},
		})
	}

	return models, nil
}
