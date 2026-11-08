package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/agenttest"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"
	"github.com/emreuyguc/native-agent-api/internal/logx"
	"github.com/emreuyguc/native-agent-api/internal/run"
	"github.com/emreuyguc/native-agent-api/internal/workspace"

	"github.com/google/uuid"
)

type modelAgent struct{ *agenttest.Agent }

func (*modelAgent) Models(context.Context) ([]agent.Model, error) {
	return []agent.Model{{
		ID: "native-model", Name: "Native Model", Efforts: []string{"medium"},
		Provider: agent.ModelProvider{Provider: agent.NewProvider("fake", nil, json.RawMessage(`{"value":"native-model"}`))},
	}}, nil
}

type readyAgent struct{ *agenttest.Agent }

func (*readyAgent) AuthStatus(context.Context) (agent.AuthStatus, error) {
	return agent.AuthStatus{
		LoggedIn: true, Account: &agent.Account{Email: "dev@example.test"},
		Provider: agent.AuthProvider{Provider: agent.NewProvider("fake", nil, json.RawMessage(`{"loggedIn":true}`))},
	}, nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()

	return config.Config{
		Network:       config.Network{Listen: "127.0.0.1:0"},
		Log:           config.Log{Format: "json"},
		HomeDir:       t.TempDir(),
		WorkDir:       t.TempDir(),
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"),
		Limits: config.Limits{
			MaxConcurrent:      4,
			QueryTimeout:       5 * time.Second,
			CommandTimeout:     5 * time.Second,
			LoginTimeout:       time.Minute,
			MaxBodyBytes:       1 << 20,
			MaxOutputBytes:     1 << 20,
			MaxFileBytes:       1 << 20,
			MaxListEntries:     100,
			RunRetention:       time.Minute,
			MaxRetainedRuns:    20,
			MaxRunEvents:       1_000,
			MaxRunObservations: 20,
			RunIdle:            time.Minute,
		},
	}
}

func newServer(t *testing.T, cfg config.Config, fake agent.Agent) *httptest.Server {
	t.Helper()

	return newServerWithLogger(t, cfg, fake, slog.New(slog.DiscardHandler))
}

func newServerWithLogger(t *testing.T, cfg config.Config, fake agent.Agent, logger *slog.Logger) *httptest.Server {
	t.Helper()
	base, cancel := context.WithCancel(context.Background())
	api, err := httpapi.New(base, cfg, fake, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		server.Close()
		cancel()
		api.Wait()
	})

	return server
}

// do sends a request and checks every JSON response against api/openapi.yaml.
func do(t *testing.T, method, url string, body io.Reader, headers ...string) (*http.Response, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = io.ReadAll(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case bytes.HasPrefix(payload, []byte("{")):
		req.Header.Set("Content-Type", "application/json")
	case len(payload) > 0:
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	checkContract(t, req, payload, resp, data)

	return resp, data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}

	return value
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("t", 32)
	cfg := testConfig(t)
	cfg.Auth = config.Auth{Enabled: true, TokenHash: config.HashToken(token)}
	server := newServer(t, cfg, &agenttest.Agent{})

	tests := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{name: "health is public", path: "/health", want: http.StatusOK},
		{name: "missing token", path: "/v1/agent", want: http.StatusUnauthorized},
		{name: "wrong token", path: "/v1/agent", header: "Bearer " + strings.Repeat("x", 32), want: http.StatusUnauthorized},
		{name: "token without scheme", path: "/v1/agent", header: token, want: http.StatusUnauthorized},
		{name: "token prefix", path: "/v1/agent", header: "Bearer " + token[:31], want: http.StatusUnauthorized},
		{name: "valid token", path: "/v1/agent", header: "Bearer " + token, want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, body := do(t, http.MethodGet, server.URL+tc.path, nil, "Authorization", tc.header)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("security headers missing")
			}
		})
	}
}

func TestWorkspaceFileLifecycle(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{})

	resp, body := do(t, http.MethodPost, server.URL+"/v1/workspaces/create", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", resp.StatusCode, body)
	}
	created := decode[workspace.Info](t, body)
	workspaceID := created.ID
	if created.Times.CreatedAt.IsZero() || created.Times.LastUsedAt != nil {
		t.Fatalf("created workspace times = %+v", created.Times)
	}
	workspaceURL := server.URL + "/v1/workspaces/" + workspaceID
	files := workspaceURL + "/files"
	resp, body = do(t, http.MethodGet, server.URL+"/v1/sessions?workspace_id="+workspaceID, nil)
	if resp.StatusCode != http.StatusOK || string(bytes.TrimSpace(body)) != `{"items":[]}` {
		t.Fatalf("list sessions: %d %s", resp.StatusCode, body)
	}

	for _, tc := range []struct {
		path, body string
		want       int
		result     string
	}{
		{"/upload/output/result.txt", "hello world", http.StatusCreated, `"path":"output/result.txt"`},
		{"/mkdir", `{"path":"archive","conflict":"error"}`, http.StatusCreated, `"path":"archive"`},
		{"/copy", `{"source":"output/result.txt","destination":"archive/result.txt","conflict":"error"}`, http.StatusCreated, `"path":"archive/result.txt"`},
		{"/copy", `{"source":"output/result.txt","destination":"archive/result.txt","conflict":"error"}`, http.StatusConflict, "path_conflict"},
		{"/move", `{"source":"archive/result.txt","destination":"archive/renamed.txt","conflict":"error"}`, http.StatusCreated, `"path":"archive/renamed.txt"`},
		{"/mkdir", `{"path":"archive"}`, http.StatusUnprocessableEntity, "property conflict"},
		{"/upload/..%2Fescape.txt", "x", http.StatusUnprocessableEntity, "invalid_path"},
	} {
		if resp, body := do(t, http.MethodPost, files+tc.path, bytes.NewBufferString(tc.body)); resp.StatusCode != tc.want || !strings.Contains(string(body), tc.result) {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, resp.StatusCode, body)
		}
	}

	resp, body = do(t, http.MethodGet, files+"/download/output/result.txt", nil, "Range", "bytes=6-")
	if resp.StatusCode != http.StatusPartialContent || string(body) != "world" {
		t.Fatalf("range download: %d %q", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, files, nil)
	listedFiles := decode[struct {
		Items []workspace.Entry `json:"items"`
	}](t, body)
	if resp.StatusCode != http.StatusOK || len(listedFiles.Items) != 4 {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}

	if resp, body = do(t, http.MethodPost, files+"/delete", bytes.NewBufferString(`{"path":"output"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete file: %d %s", resp.StatusCode, body)
	}
	if resp, body = do(t, http.MethodPost, workspaceURL+"/detach", nil); resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "wrong_workspace_storage") {
		t.Fatalf("detach a managed workspace: %d %s", resp.StatusCode, body)
	}
	if resp, _ = do(t, http.MethodPost, workspaceURL+"/delete", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete workspace: %d", resp.StatusCode)
	}
	if resp, _ = do(t, http.MethodGet, files, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list deleted workspace: %d", resp.StatusCode)
	}
}

func TestAgentEndpoints(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{})

	tests := []struct {
		path     string
		want     int
		contains string
	}{
		{path: "/v1/agent", want: http.StatusOK, contains: `"cli":{"binary":"fake","version":"1.2.3 (fake)"`},
		{path: "/v1/agent", want: http.StatusOK, contains: `"go_version"`},
		{path: "/v1/agent", want: http.StatusOK, contains: `"allowed_roots":[]`},
		{path: "/v1/agent/diagnostics", want: http.StatusNotImplemented, contains: `"unsupported"`},
		{path: "/v1/agent/status", want: http.StatusOK, contains: `"logged_in":false`},
		{path: "/v1/agent/usage", want: http.StatusNotImplemented, contains: `"unsupported"`},
		{path: "/v1/agent/models", want: http.StatusNotImplemented, contains: `"unsupported"`},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			resp, body := do(t, http.MethodGet, server.URL+tc.path, nil)
			if resp.StatusCode != tc.want || !strings.Contains(string(body), tc.contains) {
				t.Fatalf("status=%d body=%s, want %d containing %s", resp.StatusCode, body, tc.want, tc.contains)
			}
		})
	}
}

func TestAgentModels(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &modelAgent{Agent: &agenttest.Agent{}})

	resp, body := do(t, http.MethodGet, server.URL+"/v1/agent/models", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"native-model"`) ||
		!strings.Contains(string(body), `"efforts":["medium"]`) {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	// The adapter accepts "low", but the reported model does not: the catalog decides.
	resp, body = do(t, http.MethodPost, server.URL+"/v1/ask",
		strings.NewReader(`{"prompt":"hi","model":{"id":"native-model","effort":"low"}}`))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "not supported by model native-model") {
		t.Fatalf("effort validation status=%d body=%s", resp.StatusCode, body)
	}
}

type loginAgent struct {
	*agenttest.Agent
	loggedOut chan struct{}
}

func (a *loginAgent) Info() agent.Info {
	info := a.Agent.Info()
	info.LoginMethods = []string{"browser"}

	return info
}

func (*loginAgent) StartLogin(ctx context.Context, _ agent.LoginRequest) (agent.Login, error) {
	login := &fakeLogin{code: make(chan string, 1), done: make(chan error, 1)}
	go func() {
		select {
		case code := <-login.code:
			if code == "good" {
				login.done <- nil
			} else {
				login.done <- errors.New("code rejected")
			}
		case <-ctx.Done():
			login.done <- ctx.Err()
		}
	}()

	return login, nil
}

func (a *loginAgent) Logout(context.Context) error {
	close(a.loggedOut)
	return nil
}

type fakeLogin struct {
	code chan string
	done chan error
	err  error
	once sync.Once
}

func (*fakeLogin) Challenge() agent.LoginChallenge {
	return agent.LoginChallenge{Flow: agent.FlowPasteCode, URL: "https://login.example/authorize"}
}

func (l *fakeLogin) SubmitCode(code string) error {
	l.code <- code
	return nil
}

func (l *fakeLogin) Wait() error {
	l.once.Do(func() { l.err = <-l.done })
	return l.err
}

func (l *fakeLogin) Cancel() {
	select {
	case l.done <- context.Canceled:
	default:
	}
}

func TestNativeLoginFlow(t *testing.T) {
	t.Parallel()
	fake := &loginAgent{Agent: &agenttest.Agent{}, loggedOut: make(chan struct{})}
	server := newServer(t, testConfig(t), fake)
	login := server.URL + "/v1/agent/login"

	if resp, body := do(t, http.MethodGet, login, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no login yet: status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, login+"/start", strings.NewReader(`{"method":"password"}`)); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown method: status=%d body=%s", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodPost, login+"/start", strings.NewReader(`{"method":"browser"}`))
	started := decode[map[string]any](t, body)
	if resp.StatusCode != http.StatusCreated || started["status"] != "pending" || started["flow"] != "paste_code" || started["step"].(map[string]any)["url"] == "" {
		t.Fatalf("start: status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, login+"/start", strings.NewReader(`{"method":"browser"}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent start: status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, server.URL+"/v1/agent/logout", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("logout during login: status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, login+"/code", strings.NewReader(`{"code":"good"}`)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("code: status=%d body=%s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := do(t, http.MethodGet, login, nil)
		if strings.Contains(string(body), `"status":"succeeded"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("login did not succeed: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp, body := do(t, http.MethodPost, login+"/code", strings.NewReader(`{"code":"again"}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("code after success: status=%d body=%s", resp.StatusCode, body)
	}

	do(t, http.MethodPost, login+"/start", strings.NewReader(`{"method":"browser"}`))
	resp, body = do(t, http.MethodPost, login+"/cancel", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"cancelled"`) {
		t.Fatalf("cancel: status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, server.URL+"/v1/agent/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", resp.StatusCode, body)
	}
	<-fake.loggedOut
}

func TestReadinessReportsComponents(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &readyAgent{Agent: &agenttest.Agent{}})

	resp, body := do(t, http.MethodGet, server.URL+"/ready", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ready"`) ||
		!strings.Contains(string(body), `"adapter":{"status":"healthy"}`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		allowed string
		path    string
		want    int
	}{
		{name: "loopback allowed", allowed: "127.0.0.1/32", path: "/v1/agent", want: http.StatusOK},
		{name: "loopback not listed", allowed: "10.0.0.0/8", path: "/v1/agent", want: http.StatusForbidden},
		{name: "health is exempt", allowed: "10.0.0.0/8", path: "/health", want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			cfg.Network.AllowedIPs = []netip.Prefix{netip.MustParsePrefix(tc.allowed)}
			server := newServer(t, cfg, &agenttest.Agent{})
			resp, body := do(t, http.MethodGet, server.URL+tc.path, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestRequestLogging(t *testing.T) {
	t.Parallel()

	serve := func(t *testing.T, logCfg config.Log, path, body string) string {
		t.Helper()
		var buffer safeBuffer
		cfg := testConfig(t)
		cfg.Log = logCfg
		server := newServerWithLogger(t, cfg, &agenttest.Agent{}, slog.New(slog.NewJSONHandler(&buffer, nil)))
		_, _ = do(t, http.MethodPost, server.URL+path, strings.NewReader(body), "Authorization", "Bearer secret-value-123456")
		return buffer.String()
	}
	query := `{"prompt":"my key is sk-ant-api03-SECRET","model":{"id":"sonnet","effort":"low"}}`

	if out := serve(t, config.Log{Format: "json"}, "/v1/ask", query); strings.Contains(out, `"msg":"request"`) {
		t.Fatalf("access log disabled but got %q", out)
	}

	metadata := serve(t, config.Log{Format: "json", Requests: true}, "/v1/ask", query)
	if !strings.Contains(metadata, `"path":"/v1/ask"`) || strings.Contains(metadata, "request_body") {
		t.Fatalf("metadata log = %s", metadata)
	}

	bodies := serve(t, config.Log{Format: "json", Requests: true, Bodies: true, BodyLimit: 4096}, "/v1/ask", query)
	if !strings.Contains(bodies, "my key is [redacted]") || !strings.Contains(bodies, `response_body`) {
		t.Fatalf("body log = %s", bodies)
	}
	if strings.Contains(bodies, "SECRET") || strings.Contains(bodies, "secret-value") {
		t.Fatalf("secret leaked into log: %s", bodies)
	}

	files := serve(t, config.Log{Format: "json", Requests: true, Bodies: true, BodyLimit: 4096}, "/v1/workspaces/x/files/upload/a.txt", "file-content")
	if strings.Contains(files, "file-content") {
		t.Fatalf("file transfer body was logged: %s", files)
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	var buffer safeBuffer
	cfg := testConfig(t)
	cfg.Log = config.Log{Format: "json", Level: slog.LevelDebug, Requests: true}
	server := newServerWithLogger(t, cfg, &agenttest.Agent{}, logx.New(&buffer, cfg.Log))

	// Without a client ID the gateway creates a time-ordered UUIDv7.
	resp, _ := do(t, http.MethodGet, server.URL+"/v1/agent", nil)
	if id, err := uuid.Parse(resp.Header.Get("X-Request-ID")); err != nil || id.Version() != 7 {
		t.Fatalf("created request ID %q: %v", resp.Header.Get("X-Request-ID"), err)
	}

	// The client's ID is used everywhere: header, error body, the run it starts and the logs.
	resp, body := do(t, http.MethodGet, server.URL+"/v1/runs/unknown", nil, "X-Request-ID", "client-42")
	if resp.Header.Get("X-Request-ID") != "client-42" || decode[problemBody](t, body).RequestID != "client-42" {
		t.Fatalf("client request ID not used: %v %s", resp.Header, body)
	}
	_, body = do(t, http.MethodPost, server.URL+"/v1/ask", strings.NewReader(`{"prompt":"hi","model":{"id":"sonnet","effort":"low"}}`), "X-Request-ID", "client-43")
	if snapshot := decode[run.Snapshot](t, body); snapshot.Trace.RequestID != "client-43" {
		t.Fatalf("run request ID: %s", body)
	}
	logs := buffer.String()
	if !strings.Contains(logs, `"path":"/v1/runs/unknown","remote"`) || !strings.Contains(logs, `"request_id":"client-42"`) ||
		!strings.Contains(logs, `"msg":"turn finished"`) || strings.Count(logs, `"request_id":"client-43"`) < 3 {
		t.Fatalf("logs lack the request ID:\n%s", logs)
	}

	// An invalid client ID is rejected, never silently replaced.
	resp, body = do(t, http.MethodGet, server.URL+"/v1/agent", nil, "X-Request-ID", "has spaces")
	if problem := decode[problemBody](t, body); resp.StatusCode != http.StatusBadRequest || problem.Code != "invalid_request_id" ||
		problem.RequestID == "" || problem.RequestID != resp.Header.Get("X-Request-ID") {
		t.Fatalf("invalid request ID: %d %s", resp.StatusCode, body)
	}

	// Unknown paths answer with the same problem format.
	resp, body = do(t, http.MethodGet, server.URL+"/v1/nothing", nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/problem+json" ||
		decode[problemBody](t, body).Code != "not_found" {
		t.Fatalf("unknown path: %d %s", resp.StatusCode, body)
	}
}

// safeBuffer is a goroutine-safe bytes.Buffer for capturing logs.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// TestBrowserGuard covers docs/security.md F-1 (DNS rebinding) and F-2 (cross-site POST).
func TestBrowserGuard(t *testing.T) {
	t.Parallel()
	open := newServer(t, testConfig(t), &agenttest.Agent{}) // No token.
	tokenCfg := testConfig(t)
	tokenCfg.Auth = config.Auth{Enabled: true, TokenHash: sha256.Sum256([]byte(strings.Repeat("t", 32)))}
	withToken := newServer(t, tokenCfg, &agenttest.Agent{})

	send := func(base, method, path, host string, headers ...string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var problem problemBody
		_ = json.Unmarshal(body, &problem)
		return resp.StatusCode, problem.Code
	}
	port := open.URL[strings.LastIndexByte(open.URL, ':'):]
	bearer := "Bearer " + strings.Repeat("t", 32)

	cases := []struct {
		name, base, method, path, host string
		headers                        []string
		status                         int
		code                           string
	}{
		{name: "rebound name without token", base: open.URL, method: http.MethodGet, path: "/v1/workspaces", host: "evil.example" + port, status: http.StatusForbidden, code: "host_not_allowed"},
		{name: "rebound name on probe", base: open.URL, method: http.MethodGet, path: "/health", host: "evil.example" + port, status: http.StatusForbidden, code: "host_not_allowed"},
		{name: "localhost without token", base: open.URL, method: http.MethodGet, path: "/v1/workspaces", host: "localhost" + port, status: http.StatusOK},
		{name: "ipv6 literal without token", base: open.URL, method: http.MethodGet, path: "/v1/workspaces", host: "[::1]" + port, status: http.StatusOK},
		{name: "name with token", base: withToken.URL, method: http.MethodGet, path: "/v1/workspaces", host: "gateway.internal", headers: []string{"Authorization", bearer}, status: http.StatusOK},
		{name: "cross-site form post", base: open.URL, method: http.MethodPost, path: "/v1/workspaces/create", headers: []string{"Origin", "https://evil.example"}, status: http.StatusForbidden, code: "cross_origin_request"},
		{name: "opaque origin", base: open.URL, method: http.MethodPost, path: "/v1/agent/logout", headers: []string{"Origin", "null"}, status: http.StatusForbidden, code: "cross_origin_request"},
		{name: "same-site other port", base: open.URL, method: http.MethodPost, path: "/v1/workspaces/create", headers: []string{"Origin", "http://127.0.0.1:1"}, status: http.StatusForbidden, code: "cross_origin_request"},
		{name: "fetch metadata only", base: open.URL, method: http.MethodPost, path: "/v1/workspaces/create", headers: []string{"Sec-Fetch-Site", "cross-site"}, status: http.StatusForbidden, code: "cross_origin_request"},
		{name: "cross-site even with token", base: withToken.URL, method: http.MethodPost, path: "/v1/workspaces/create", headers: []string{"Authorization", bearer, "Origin", "https://evil.example"}, status: http.StatusForbidden, code: "cross_origin_request"},
		{name: "same origin post", base: open.URL, method: http.MethodPost, path: "/v1/workspaces/create", headers: []string{"Origin", open.URL}, status: http.StatusCreated},
		{name: "client without origin", base: open.URL, method: http.MethodPost, path: "/v1/workspaces/create", status: http.StatusCreated},
		{name: "cross-site read", base: open.URL, method: http.MethodGet, path: "/v1/workspaces", headers: []string{"Origin", "https://evil.example"}, status: http.StatusOK},
	}
	for _, tc := range cases {
		status, code := send(tc.base, tc.method, tc.path, tc.host, tc.headers...)
		if status != tc.status || code != tc.code {
			t.Errorf("%s: %d %q, want %d %q", tc.name, status, code, tc.status, tc.code)
		}
	}
}
