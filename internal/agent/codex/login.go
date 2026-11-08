package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// Native login methods. Device code works from any machine; chatgpt needs the browser on the
// gateway host because the native callback listens on localhost.
const (
	LoginChatGPTDeviceCode = "chatgpt_device_code"
	LoginChatGPT           = "chatgpt"
)

var errLoginFailed = errors.New("codex login failed")

var _ agent.Authenticator = (*Agent)(nil)

// StartLogin runs account/login/start on a dedicated app-server process that stays alive
// until account/login/completed arrives.
func (a *Agent) StartLogin(ctx context.Context, req agent.LoginRequest) (agent.Login, error) {
	params := map[string]any{}
	switch req.Method {
	case LoginChatGPTDeviceCode:
		params["type"] = "chatgptDeviceCode"
	case LoginChatGPT:
		params["type"] = "chatgpt"
	default:
		return nil, agent.Invalid("method must be %s or %s", LoginChatGPTDeviceCode, LoginChatGPT)
	}
	if len(req.Options) > 0 {
		return nil, agent.Invalid("provider: Codex login has no options")
	}

	ctx, cancel := context.WithCancel(ctx)
	cmd := commandProcess(ctx, a.opts.Binary, "app-server", "--listen", "stdio://")
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stderr := iox.NewTailBuffer(16_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		cancel()
		return nil, fmt.Errorf("open Codex login pipes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Codex login: %w", err)
	}
	login := &codexLogin{cmd: cmd, stdin: stdin, cancel: cancel, stderr: stderr, done: make(chan struct{})}
	transport := boundedRPC{scanner: bufio.NewScanner(stdout), writer: stdin, limit: a.opts.MaxOutputBytes}
	transport.scanner.Buffer(make([]byte, 64<<10), int(a.opts.MaxOutputBytes))
	abort := func(err error) (agent.Login, error) {
		cancel()
		_ = stdin.Close()
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()

		return nil, err
	}
	if _, err := transport.call(1, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "native_agent_api", "title": "Native Agent API", "version": "0",
	}}); err != nil {
		return abort(err)
	}
	if err := transport.write(map[string]any{"method": "initialized"}); err != nil {
		return abort(err)
	}
	result, err := transport.call(2, "account/login/start", params)
	if err != nil {
		return abort(err)
	}
	var started struct {
		LoginID         string `json:"loginId"`
		AuthURL         string `json:"authUrl"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := json.Unmarshal(result, &started); err != nil || started.LoginID == "" ||
		(started.AuthURL == "" && started.VerificationURL == "") {
		return abort(errors.Join(agent.ErrUpstream, errors.New("codex returned an incomplete login challenge")))
	}
	login.loginID = started.LoginID
	login.challenge = agent.LoginChallenge{Flow: agent.FlowBrowser, URL: started.AuthURL}
	if req.Method == LoginChatGPTDeviceCode {
		login.challenge = agent.LoginChallenge{Flow: agent.FlowDeviceCode, URL: started.VerificationURL, UserCode: started.UserCode}
		if login.challenge.URL == "" {
			login.challenge.URL = started.AuthURL
		}
	}
	go login.run(&transport, stdout)

	return login, nil
}

// Logout implements agent.Authenticator with the native `codex logout` command.
func (a *Agent) Logout(ctx context.Context) error {
	_, err := a.exec(ctx, "logout")
	return err
}

type codexLogin struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	stderr    *iox.TailBuffer
	loginID   string
	challenge agent.LoginChallenge
	done      chan struct{}
	err       error
}

// run waits for the matching completion notification, then stops the process.
func (l *codexLogin) run(transport *boundedRPC, stdout io.Reader) {
	l.err = l.awaitCompletion(transport)
	_ = l.stdin.Close()
	l.cancel()
	_, _ = io.Copy(io.Discard, stdout)
	_ = l.cmd.Wait()
	if l.err != nil && errors.Is(l.err, errProcessEnded) {
		if message := redact.Message(l.stderr.String(), maxErrorMessage); message != "" {
			l.err = fmt.Errorf("%w: %s", errLoginFailed, message)
		}
	}
	close(l.done)
}

func (l *codexLogin) awaitCompletion(transport *boundedRPC) error {
	for transport.scanner.Scan() {
		var message struct {
			Method string `json:"method"`
			Params struct {
				LoginID *string `json:"loginId"`
				Success bool    `json:"success"`
				Error   *string `json:"error"`
			} `json:"params"`
		}
		if json.Unmarshal(transport.scanner.Bytes(), &message) != nil || message.Method != "account/login/completed" {
			continue
		}
		if message.Params.LoginID != nil && *message.Params.LoginID != l.loginID {
			continue
		}
		if message.Params.Success {
			return nil
		}
		reason := "the native login did not succeed"
		if message.Params.Error != nil {
			reason = redact.Message(*message.Params.Error, maxErrorMessage)
		}

		return fmt.Errorf("%w: %s", errLoginFailed, reason)
	}

	return fmt.Errorf("%w: %w", errLoginFailed, errProcessEnded)
}

func (l *codexLogin) Challenge() agent.LoginChallenge { return l.challenge }

func (*codexLogin) SubmitCode(string) error {
	return agent.Invalid("codex login completes in the browser; no code is submitted")
}

func (l *codexLogin) Wait() error {
	<-l.done
	return l.err
}

func (l *codexLogin) Cancel() { l.cancel() }
