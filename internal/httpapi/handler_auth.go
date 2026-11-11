package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Login statuses.
const (
	loginPending   = "pending"
	loginSucceeded = "succeeded"
	loginFailed    = "failed"
	loginCancelled = "cancelled"
	loginExpired   = "expired"
)

// logins holds at most one native login. Native CLIs write a single credential store, so
// concurrent logins would race; a finished login stays readable until the next one starts.
type logins struct {
	// starting serializes native login starts and logout without blocking progress reads.
	starting sync.Mutex
	mu       sync.Mutex
	current  *loginEntry
}

type loginEntry struct {
	view   loginView
	login  agent.Login
	cancel context.CancelFunc
	// cancelled distinguishes an explicit DELETE from a failure or timeout.
	cancelled bool
	// settled is closed once view holds the final status.
	settled chan struct{}
}

// loginView is the progress of a native login.
type loginView struct {
	Method string `json:"method" doc:"Native login method, one of the agent's login.methods."`
	Status string `json:"status" enum:"pending,succeeded,failed,cancelled,expired"`
	Error  string `json:"error,omitempty" doc:"Why the login failed or expired."`
	// Flow says what the user does: paste_code (open url, submit the code the page shows),
	// device_code (open url, enter user_code) or browser (open url and finish there).
	Flow  string     `json:"flow" enum:"paste_code,device_code,browser"`
	Step  loginStep  `json:"step"`
	Times loginTimes `json:"times"`
}

// loginStep is what the user has to do now.
type loginStep struct {
	URL      string `json:"url" doc:"Sign-in page to open in any browser."`
	UserCode string `json:"user_code,omitempty" doc:"Code to enter on the sign-in page (flow device_code)."`
	// Code tracks the code the page shows (flow paste_code only).
	Code *loginCode `json:"code,omitempty"`
}

type loginCode struct {
	Submitted bool `json:"submitted" doc:"A code was submitted with POST /v1/agent/login/code."`
}

type loginTimes struct {
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func newLoginView(method string, challenge agent.LoginChallenge, created, expires time.Time) loginView {
	view := loginView{
		Method: method, Status: loginPending, Flow: challenge.Flow,
		Step:  loginStep{URL: challenge.URL, UserCode: challenge.UserCode},
		Times: loginTimes{CreatedAt: created, ExpiresAt: expires},
	}
	if challenge.Flow == agent.FlowPasteCode {
		view.Step.Code = &loginCode{}
	}

	return view
}

type startLoginInput struct {
	Body *struct {
		Method   string        `json:"method" doc:"Native login method; one of the agent's login.methods."`
		Provider loginProvider `json:"provider,omitempty"`
	}
}

type submitCodeInput struct {
	Body *struct {
		Code string `json:"code" maxLength:"4096" doc:"Code shown by the sign-in page."`
	}
}

func (s *Server) registerAuth(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "getLoginStatus", Method: http.MethodGet, Path: "/v1/agent/status",
		Summary: "Whether the CLI is logged in, with its own status document", Errors: []int{401, 409, 502},
	}, s.agentAuthStatus)
	huma.Register(api, huma.Operation{
		OperationID: "startLogin", Method: http.MethodPost, Path: "/v1/agent/login/start",
		Summary:       "Start the native login flow",
		Description:   "Runs the CLI's own login. Open `step.url` in any browser; with `flow: paste_code`, submit the code the page shows; with `device_code`, enter `step.user_code` on the page. Only one login runs at a time.",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, Errors: []int{409, 422, 501, 502},
	}, s.startLogin)
	huma.Register(api, huma.Operation{
		OperationID: "getLogin", Method: http.MethodGet, Path: "/v1/agent/login",
		Summary: "Progress of the current or latest login", Errors: []int{404},
	}, s.getLogin)
	huma.Register(api, huma.Operation{
		OperationID: "submitLoginCode", Method: http.MethodPost, Path: "/v1/agent/login/code",
		Summary: "Submit the code shown by the sign-in page", DefaultStatus: http.StatusAccepted,
		MaxBodyBytes: 8 << 10, Errors: []int{404, 409, 422},
	}, s.submitLoginCode)
	huma.Register(api, huma.Operation{
		OperationID: "cancelLogin", Method: http.MethodPost, Path: "/v1/agent/login/cancel",
		Summary: "Cancel the pending login", Errors: []int{404},
	}, s.cancelLogin)
	huma.Register(api, huma.Operation{
		OperationID: "logout", Method: http.MethodPost, Path: "/v1/agent/logout",
		Summary: "Remove the native CLI credentials", DefaultStatus: http.StatusNoContent, Errors: []int{409, 501, 502},
	}, s.logout)
}

func (s *Server) startLogin(ctx context.Context, in *startLoginInput) (*body[loginView], error) {
	authenticator, ok := s.agent.(agent.Authenticator)
	if !ok {
		return nil, agentError(agent.ErrUnsupported)
	}
	if in.Body == nil || !slices.Contains(s.agent.Info().LoginMethods, in.Body.Method) {
		return nil, fail(http.StatusUnprocessableEntity, codeInvalidRequest, "method must be one of the agent's login.methods")
	}
	options, err := agent.SplitProvider(json.RawMessage(in.Body.Provider), "provider", s.agent.Name())
	if err != nil {
		return nil, agentError(err)
	}
	if !s.logins.starting.TryLock() {
		return nil, fail(http.StatusConflict, codeLoginInProgress, "Another login is starting.")
	}
	defer s.logins.starting.Unlock()
	if s.loginPending() {
		return nil, fail(http.StatusConflict, codeLoginInProgress, "Another login is pending; cancel it or wait for it to finish.")
	}
	created := time.Now().UTC()
	// The login outlives this request: it is bounded by the server lifetime and LoginTimeout.
	loginCtx, cancel := context.WithTimeout(s.base, s.cfg.Limits.LoginTimeout)
	request := agent.LoginRequest{Method: in.Body.Method, Options: options}
	login, err := authenticator.StartLogin(loginCtx, request) //nolint:contextcheck // Detached on purpose; see above.
	if err != nil {
		cancel()
		s.log.WarnContext(ctx, "native login failed to start", "method", in.Body.Method, "err", err)
		return nil, agentError(err)
	}
	entry := &loginEntry{
		login: login, cancel: cancel, settled: make(chan struct{}),
		view: newLoginView(in.Body.Method, login.Challenge(), created, created.Add(s.cfg.Limits.LoginTimeout)),
	}
	s.logins.mu.Lock()
	s.logins.current = entry
	s.logins.mu.Unlock()
	go s.finishLogin(loginCtx, entry) //nolint:contextcheck // Detached on purpose; see above.
	s.log.InfoContext(ctx, "native login started", "method", in.Body.Method)

	return reply(entry.view), nil
}

func (s *Server) finishLogin(loginCtx context.Context, entry *loginEntry) {
	err := entry.login.Wait()
	expired := errors.Is(loginCtx.Err(), context.DeadlineExceeded)
	entry.cancel()
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()
	finished := time.Now().UTC()
	entry.view.Times.FinishedAt = &finished
	switch {
	case err == nil:
		entry.view.Status = loginSucceeded
	case entry.cancelled:
		entry.view.Status = loginCancelled
	case expired:
		entry.view.Status, entry.view.Error = loginExpired, "The login was not completed in time."
	default:
		entry.view.Status, entry.view.Error = loginFailed, err.Error()
	}
	close(entry.settled)
	s.log.Info("native login finished", "method", entry.view.Method, "status", entry.view.Status)
}

func (s *Server) loginPending() bool {
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()

	return s.logins.current != nil && s.logins.current.view.Status == loginPending
}

// lookupLogin returns the current or latest login; the caller holds s.logins.mu.
func (s *Server) lookupLogin() (*loginEntry, error) {
	if current := s.logins.current; current != nil {
		return current, nil
	}

	return nil, fail(http.StatusNotFound, codeLoginNotFound, "No login has been started.")
}

func (s *Server) getLogin(context.Context, *struct{}) (*body[loginView], error) {
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()
	entry, err := s.lookupLogin()
	if err != nil {
		return nil, err
	}

	return reply(entry.view), nil
}

func (s *Server) submitLoginCode(_ context.Context, in *submitCodeInput) (*body[loginView], error) {
	if in.Body == nil {
		return nil, fail(http.StatusUnprocessableEntity, codeInvalidRequest, "code is required")
	}
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()
	entry, err := s.lookupLogin()
	if err != nil {
		return nil, err
	}
	if entry.view.Status != loginPending {
		return nil, fail(http.StatusConflict, codeLoginFinished, "The login has already finished.")
	}
	if err := entry.login.SubmitCode(in.Body.Code); err != nil {
		return nil, agentError(err)
	}
	if entry.view.Step.Code != nil {
		entry.view.Step.Code.Submitted = true
	}

	return reply(entry.view), nil
}

func (s *Server) cancelLogin(context.Context, *struct{}) (*body[loginView], error) {
	s.logins.mu.Lock()
	entry, err := s.lookupLogin()
	if err != nil {
		s.logins.mu.Unlock()
		return nil, err
	}
	if entry.view.Status == loginPending {
		entry.cancelled = true
		entry.login.Cancel()
	}
	s.logins.mu.Unlock()
	<-entry.settled // Report the final state, not a transient one.
	s.logins.mu.Lock()
	defer s.logins.mu.Unlock()

	return reply(entry.view), nil
}

func (s *Server) logout(ctx context.Context, _ *struct{}) (*struct{}, error) {
	authenticator, ok := s.agent.(agent.Authenticator)
	if !ok {
		return nil, agentError(agent.ErrUnsupported)
	}
	if !s.logins.starting.TryLock() {
		return nil, fail(http.StatusConflict, codeLoginInProgress, "A login is starting.")
	}
	defer s.logins.starting.Unlock()
	if s.loginPending() {
		return nil, fail(http.StatusConflict, codeLoginInProgress, "Cancel the pending login before logging out.")
	}
	if err := authenticator.Logout(ctx); err != nil {
		return nil, agentError(err)
	}
	s.log.InfoContext(ctx, "native logout completed")

	return nil, nil //nolint:nilnil // 204 No Content.
}
