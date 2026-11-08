package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/iox"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// Native login methods: a Claude subscription or Anthropic Console (API billing).
const (
	LoginClaudeAI = "claudeai"
	LoginConsole  = "console"
)

var (
	loginURLPattern   = regexp.MustCompile(`https://[^\s\x1b\x07]+`)
	loginEmailPattern = regexp.MustCompile(`^[^\s@]{1,128}@[^\s@]{1,253}$`)
	errLoginFailed    = errors.New("claude login failed")
	errCodeSubmitted  = errors.New("a code was already submitted for this login")
)

var _ agent.Authenticator = (*Agent)(nil)

// StartLogin runs `claude auth login` without a terminal. The CLI prints the sign-in URL and
// reads the code shown by the sign-in page from standard input.
func (a *Agent) StartLogin(ctx context.Context, req agent.LoginRequest) (agent.Login, error) {
	args := []string{"auth", "login"}
	switch req.Method {
	case LoginClaudeAI:
		args = append(args, "--claudeai")
	case LoginConsole:
		args = append(args, "--console")
	default:
		return nil, agent.Invalid("method must be %s or %s", LoginClaudeAI, LoginConsole)
	}
	var options LoginOptions
	if err := agent.DecodeStrict(req.Options, "provider", &options); err != nil {
		return nil, err
	}
	if options.SSO {
		args = append(args, "--sso")
	}
	if options.Email != "" {
		if !loginEmailPattern.MatchString(options.Email) {
			return nil, agent.Invalid("provider.email is not a valid address")
		}
		args = append(args, "--email", options.Email)
	}

	ctx, cancel := context.WithCancel(ctx)
	cmd := a.command(ctx, args...)
	cmd.Dir, cmd.Env = a.opts.WorkDir, Environment(a.opts)
	stderr := iox.NewTailBuffer(16_000)
	cmd.Stderr = stderr
	stdin, stdinErr := cmd.StdinPipe()
	stdout, stdoutErr := cmd.StdoutPipe()
	if err := errors.Join(stdinErr, stdoutErr); err != nil {
		cancel()
		return nil, fmt.Errorf("open claude login pipes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude login: %w", err)
	}
	login := &claudeLogin{
		cmd: cmd, stdin: stdin, cancel: cancel, stdout: iox.NewTailBuffer(16_000), stderr: stderr,
		done: make(chan struct{}),
	}
	urls := make(chan string, 1)
	go login.run(stdout, urls)

	select {
	case url := <-urls:
		login.challenge = agent.LoginChallenge{Flow: agent.FlowPasteCode, URL: url}
		return login, nil
	case <-login.done:
		return nil, login.failure()
	case <-ctx.Done():
		login.Cancel()
		<-login.done
		return nil, fmt.Errorf("claude login did not report a sign-in URL: %w", ctx.Err())
	}
}

// Logout implements agent.Authenticator with `claude auth logout`.
func (a *Agent) Logout(ctx context.Context) error {
	_, err := a.exec(ctx, "auth", "logout")
	return err
}

type claudeLogin struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	stdout    *iox.TailBuffer // Written only by run; read after done.
	stderr    *iox.TailBuffer
	challenge agent.LoginChallenge
	done      chan struct{}
	err       error

	mu        sync.Mutex
	submitted bool
}

// run forwards the first sign-in URL, keeps the rest of stdout for error messages and reaps
// the process only after stdout is drained, as exec.Cmd requires.
func (l *claudeLogin) run(stdout io.Reader, urls chan<- string) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 16<<10), 64<<10)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if !found && strings.Contains(line, "oauth/authorize") {
			if url := loginURLPattern.FindString(line); url != "" {
				found = true
				urls <- url
				continue
			}
		}
		// Keep only what explains a failure, not the CLI's own prompts.
		line = strings.TrimSpace(strings.TrimPrefix(line, "Paste code here if prompted >"))
		if line == "" || strings.HasPrefix(line, "Opening browser") {
			continue
		}
		_, _ = l.stdout.Write([]byte(line + "\n"))
	}
	_, _ = io.Copy(io.Discard, stdout)
	l.err = l.cmd.Wait()
	l.cancel()
	close(l.done)
}

func (l *claudeLogin) Challenge() agent.LoginChallenge { return l.challenge }

func (l *claudeLogin) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > agent.MaxLoginCode || strings.ContainsAny(code, "\r\n\x00") {
		return agent.Invalid("code must be 1-%d bytes on a single line", agent.MaxLoginCode)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.submitted {
		return agent.Invalid("%v", errCodeSubmitted)
	}
	select {
	case <-l.done:
		return agent.Invalid("the login has already finished")
	default:
	}
	l.submitted = true
	if _, err := io.WriteString(l.stdin, code+"\n"); err != nil {
		return fmt.Errorf("send code to claude login: %w", err)
	}

	return nil
}

func (l *claudeLogin) Wait() error {
	<-l.done
	if l.err == nil {
		return nil
	}

	return l.failure()
}

func (l *claudeLogin) failure() error {
	message := redact.Message(strings.TrimSpace(l.stdout.String()+"\n"+l.stderr.String()), maxErrorMessage)
	if message == "" {
		return errLoginFailed
	}

	return fmt.Errorf("%w: %s", errLoginFailed, message)
}

func (l *claudeLogin) Cancel() { l.cancel() }
