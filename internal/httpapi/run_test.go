package httpapi_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/agenttest"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"
	"github.com/emreuyguc/native-agent-api/internal/run"
	"github.com/emreuyguc/native-agent-api/internal/session"
	"github.com/emreuyguc/native-agent-api/internal/workspace"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// modelJSON is the model group every run needs; the fake agent accepts effort "low".
const modelJSON = `"model":{"id":"sonnet","effort":"low"}`

const bypassJSON = `,"tools":{"permissions":"bypass"}`

func TestAsk(t *testing.T) {
	t.Parallel()
	fake := &agenttest.Agent{}
	server := newServer(t, testConfig(t), fake)

	resp, body := do(t, http.MethodPost, server.URL+"/v1/ask", strings.NewReader(`{"prompt":"hi",`+modelJSON+`}`))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Run-ID") == "" {
		t.Fatalf("ask: %d %s", resp.StatusCode, body)
	}
	snapshot := decode[run.Snapshot](t, body)
	if snapshot.Status != run.StatusSucceeded || !snapshot.Mode.Stateless || snapshot.Result == nil ||
		snapshot.Result.Text != "echo: hi" || snapshot.Context.SessionID != "" || snapshot.Mode.Interactive {
		t.Fatalf("unexpected response %s", body)
	}
	if got := fake.LastRequest(); !got.Stateless() || got.WorkspaceDir != "" || got.SessionID != "" {
		t.Fatalf("a call without a workspace kept state: %+v", got)
	}

	resp, body = do(t, http.MethodPost, server.URL+"/v1/ask/stream", strings.NewReader(`{"prompt":"hi",`+modelJSON+`}`))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	text := string(body)
	if strings.Count(text, "event: agent\n") != 3 || !strings.Contains(text, "id: 4\nevent: turn\n") ||
		!strings.Contains(text, "id: 5\nevent: done\n") || resp.Header.Get("X-Run-ID") == "" {
		t.Fatalf("unexpected SSE body:\n%s", text)
	}
}

func TestAskRejectsBadInput(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{})
	_, body := do(t, http.MethodPost, server.URL+"/v1/workspaces/create", nil)
	workspaceID := decode[workspace.Info](t, body).ID

	tests := []struct {
		name, body, code, location string
		want                       int
	}{
		{name: "unknown field", body: `{"prompt":"hi",` + modelJSON + `,"shell":"rm -rf /"}`, want: http.StatusUnprocessableEntity, code: "invalid_request", location: "body.shell"},
		{name: "flat model field", body: `{"prompt":"hi","model":"sonnet","effort":"low"}`, want: http.StatusUnprocessableEntity, code: "invalid_request", location: "body.model"},
		{name: "two objects", body: `{"prompt":"hi",` + modelJSON + `}{}`, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "model required", body: `{"prompt":"hi"}`, want: http.StatusUnprocessableEntity, code: "invalid_request", location: "body"},
		{name: "agent validation", body: `{"prompt":"hi","model":{"id":"sonnet","effort":"turbo"}}`, want: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "tools without workspace", body: `{"prompt":"hi",` + modelJSON + bypassJSON + `}`, want: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "workspace without tools", body: `{"prompt":"hi",` + modelJSON + `,"workspace_id":"` + workspaceID + `"}`, want: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "ask permissions need a stream", body: `{"prompt":"hi",` + modelJSON + `,"workspace_id":"` + workspaceID + `","tools":{"permissions":"ask"}}`, want: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "options for another provider", body: `{"prompt":"hi",` + modelJSON + `,"provider":{"name":"codex","service_tier":"fast"}}`, want: http.StatusUnprocessableEntity, code: "provider_mismatch"},
		{name: "provider section without name", body: `{"prompt":"hi",` + modelJSON + `,"provider":{"max_turns":2}}`, want: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "old options field", body: `{"prompt":"hi",` + modelJSON + `,"options":{}}`, want: http.StatusUnprocessableEntity, code: "invalid_request", location: "body.options"},
		{name: "unknown workspace", body: `{"prompt":"hi",` + modelJSON + bypassJSON + `,"workspace_id":"` + strings.Repeat("a", 32) + `"}`, want: http.StatusNotFound, code: "not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, body := do(t, http.MethodPost, server.URL+"/v1/ask", strings.NewReader(tc.body))
			problem := decode[problemBody](t, body)
			if resp.StatusCode != tc.want || problem.Status != tc.want || problem.Code != tc.code ||
				resp.Header.Get("Content-Type") != "application/problem+json" || problem.RequestID != resp.Header.Get("X-Request-ID") {
				t.Fatalf("status = %d, want %d %s (%s)", resp.StatusCode, tc.want, tc.code, body)
			}
			if tc.location != "" && !problem.hasLocation(tc.location) {
				t.Fatalf("errors do not name %s: %s", tc.location, body)
			}
			// Validation errors name the field but never echo the submitted values.
			if strings.Contains(string(body), "rm -rf") || strings.Contains(string(body), `"hi"`) {
				t.Fatalf("error echoes the request: %s", body)
			}
		})
	}
}

// problemBody is an RFC 9457 error response.
type problemBody struct {
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
	Errors    []struct {
		Location string `json:"location"`
	} `json:"errors"`
}

func (p problemBody) hasLocation(prefix string) bool {
	for _, field := range p.Errors {
		if strings.HasPrefix(field.Location, prefix) {
			return true
		}
	}

	return false
}

// newWorkspace creates a managed workspace and returns its ID.
func newWorkspace(t *testing.T, serverURL string) string {
	t.Helper()
	resp, body := do(t, http.MethodPost, serverURL+"/v1/workspaces/create", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", resp.StatusCode, body)
	}

	return decode[workspace.Info](t, body).ID
}

// startRun starts a conversation in workspaceID. extra holds further JSON members; without a
// tools group the run bypasses permissions.
func startRun(t *testing.T, serverURL, workspaceID, extra string) run.Snapshot {
	t.Helper()
	if !strings.Contains(extra, `"tools"`) {
		extra = bypassJSON + extra
	}
	payload := `{"prompt":"hi",` + modelJSON + `,"workspace_id":"` + workspaceID + `"` + extra + `}`
	resp, body := do(t, http.MethodPost, serverURL+"/v1/runs/start", strings.NewReader(payload))
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Location") == "" {
		t.Fatalf("start run: %d %s", resp.StatusCode, body)
	}
	snapshot := decode[run.Snapshot](t, body)
	if snapshot.Context.SessionID == "" || !snapshot.Mode.Interactive {
		t.Fatalf("start run without a session: %s", body)
	}

	return snapshot
}

// createWorkspaceRun creates a workspace, starts a run with the extra JSON members and returns its URL.
func createWorkspaceRun(t *testing.T, serverURL, extra string) string {
	t.Helper()
	return serverURL + "/v1/runs/" + startRun(t, serverURL, newWorkspace(t, serverURL), extra).ID
}

// pollRun waits until the run snapshot satisfies cond.
func pollRun(t *testing.T, runURL string, cond func(run.Snapshot) bool) run.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := do(t, http.MethodGet, runURL, nil)
		snapshot := decode[run.Snapshot](t, body)
		if cond(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finish closes a run after its current turn and waits for the end.
func finish(t *testing.T, runURL string) run.Snapshot {
	t.Helper()
	if resp, body := do(t, http.MethodPost, runURL+"/close", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("close: %d %s", resp.StatusCode, body)
	}

	return pollRun(t, runURL, func(s run.Snapshot) bool { return s.Status.Terminal() })
}

// continueSession continues a session, waiting for the previous run to release it.
func continueSession(t *testing.T, serverURL, sessionID, payload string) (*http.Response, []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, body := do(t, http.MethodPost, serverURL+"/v1/sessions/"+sessionID+"/continue", strings.NewReader(payload))
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "session_busy") || time.Now().After(deadline) {
			return resp, body
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunLifecycleWithResumableSSE(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{})
	runURL := createWorkspaceRun(t, server.URL, "")
	pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	finish(t, runURL)

	// Full stream, then resume after the first event.
	_, full := do(t, http.MethodGet, runURL+"/events", nil)
	if !strings.Contains(string(full), "id: 1\nevent: agent\n") || !strings.Contains(string(full), "event: done\n") {
		t.Fatalf("full stream:\n%s", full)
	}
	_, resumed := do(t, http.MethodGet, runURL+"/events", nil, "Last-Event-ID", "1")
	if strings.Contains(string(resumed), "id: 1\n") || !strings.Contains(string(resumed), "id: 2\n") {
		t.Fatalf("resumed stream:\n%s", resumed)
	}

	resp, body := do(t, http.MethodGet, server.URL+"/v1/runs", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"items":[{`) || strings.Contains(string(body), "next_cursor") {
		t.Fatalf("list runs: %d %s", resp.StatusCode, body)
	}
	if resp, body = do(t, http.MethodGet, server.URL+"/v1/runs?cursor=unknown", nil); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "invalid_cursor") {
		t.Fatalf("unknown cursor: %d %s", resp.StatusCode, body)
	}
	if resp, _ = do(t, http.MethodGet, server.URL+"/v1/runs/unknown", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run: %d", resp.StatusCode)
	}
	if resp, _ = do(t, http.MethodGet, runURL+"/events?after=-1", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad resume position: %d", resp.StatusCode)
	}
}

func TestRunSurvivesClientDisconnectAndCancelsViaREST(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	fake := &agenttest.Agent{Script: agenttest.Blocking(release), Started: make(chan agent.Request, 1)}
	server := newServer(t, testConfig(t), fake)
	runURL := createWorkspaceRun(t, server.URL, "")
	<-fake.Started

	// A reader that goes away must not stop a detached run.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, runURL+"/events", http.NoBody)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	cancel()
	if _, body := do(t, http.MethodGet, runURL, nil); decode[run.Snapshot](t, body).Status != run.StatusRunning {
		t.Fatalf("run stopped after reader disconnect: %s", body)
	}

	if resp, _ := do(t, http.MethodPost, runURL+"/cancel", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	_, events := do(t, http.MethodGet, runURL+"/events", nil)
	if !strings.Contains(string(events), "event: error\n") || !strings.Contains(string(events), `"code":"cancelled"`) {
		t.Fatalf("events after cancel:\n%s", events)
	}
}

func TestRunWebSocketStreamsAndCancels(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	fake := &agenttest.Agent{Script: agenttest.Blocking(release), Started: make(chan agent.Request, 1)}
	server := newServer(t, testConfig(t), fake)
	runURL := createWorkspaceRun(t, server.URL, "")
	<-fake.Started

	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(runURL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if err := wsjson.Write(t.Context(), conn, map[string]string{"type": "bogus"}); err != nil {
		t.Fatal(err)
	}
	for {
		var reply map[string]any
		if err := wsjson.Read(t.Context(), conn, &reply); err != nil {
			t.Fatal(err)
		}
		if reply["type"] == "control_error" {
			break
		}
	}
	if err := wsjson.Write(t.Context(), conn, map[string]string{"type": "cancel"}); err != nil {
		t.Fatal(err)
	}
	var last run.Event
	for {
		var event run.Event
		if err := wsjson.Read(t.Context(), conn, &event); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("read: %v", err)
			}
			break
		}
		last = event
	}
	if last.Type != run.EventError || !strings.Contains(string(last.Data), "cancelled") {
		t.Fatalf("last event = %+v", last)
	}
}

func TestWorkspaceRunLocksSession(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	fake := &agenttest.Agent{Script: agenttest.Blocking(release), Started: make(chan agent.Request, 1)}
	server := newServer(t, testConfig(t), fake)
	workspaceID := newWorkspace(t, server.URL)
	started := startRun(t, server.URL, workspaceID, "")
	runURL, sessionURL := server.URL+"/v1/runs/"+started.ID, server.URL+"/v1/sessions/"+started.Context.SessionID
	<-fake.Started

	if resp, body := do(t, http.MethodPost, sessionURL+"/delete", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete active session: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, sessionURL+"/continue", strings.NewReader(`{"prompt":"again"}`)); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(body), "session_busy") {
		t.Fatalf("concurrent session run: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, http.MethodPost, server.URL+"/v1/workspaces/"+workspaceID+"/delete", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete busy workspace: %d", resp.StatusCode)
	}
	if dir := fake.LastRequest().WorkspaceDir; !strings.HasSuffix(dir, workspaceID) {
		t.Fatalf("WorkspaceDir = %q", dir)
	}

	close(release)
	finish(t, runURL)
	_, events := do(t, http.MethodGet, runURL+"/events", nil)
	scanner := bufio.NewScanner(strings.NewReader(string(events)))
	done := false
	for scanner.Scan() {
		done = done || scanner.Text() == "event: done"
	}
	if !done {
		t.Fatalf("run did not finish:\n%s", events)
	}
	if resp, body := do(t, http.MethodGet, sessionURL, nil); resp.StatusCode != http.StatusOK || decode[session.View](t, body).State != session.StateCompleted {
		t.Fatalf("completed session metadata: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, sessionURL+"/delete", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete completed session: %d %s", resp.StatusCode, body)
	}
	// The lock is released when the run finishes.
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for {
		resp, _ = do(t, http.MethodPost, server.URL+"/v1/workspaces/"+workspaceID+"/delete", nil)
		if resp.StatusCode == http.StatusNoContent || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after run: %d", resp.StatusCode)
	}
}

func TestContinueAndForkSession(t *testing.T) {
	t.Parallel()
	fake := &agenttest.Agent{}
	server := newServer(t, testConfig(t), fake)
	workspaceID := newWorkspace(t, server.URL)
	started := startRun(t, server.URL, workspaceID, `,"instructions":{"system_prompt":"be brief"}`)
	finish(t, server.URL+"/v1/runs/"+started.ID)

	// Only the prompt is needed: the session remembers its settings.
	resp, body := continueSession(t, server.URL, started.Context.SessionID, `{"prompt":"next"}`)
	if resp.StatusCode != http.StatusAccepted || decode[run.Snapshot](t, body).Context.SessionID != started.Context.SessionID {
		t.Fatalf("continue: %d %s", resp.StatusCode, body)
	}
	finish(t, server.URL+"/v1/runs/"+decode[run.Snapshot](t, body).ID)
	got := fake.LastRequest()
	if got.SessionMode != agent.SessionResume || got.NativeSessionID != started.Context.SessionID || got.Model != "sonnet" ||
		got.Effort != "low" || got.Permissions != agent.PermissionsBypass || got.SystemPrompt != "be brief" || got.Prompt != "next" {
		t.Fatalf("continue request = %+v", got)
	}

	// A supplied field replaces the remembered one; the rest is kept.
	resp, body = continueSession(t, server.URL, started.Context.SessionID, `{"prompt":"more","instructions":{"system_prompt":""},"tools":{"permissions":"ask"}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("continue with overrides: %d %s", resp.StatusCode, body)
	}
	finish(t, server.URL+"/v1/runs/"+decode[run.Snapshot](t, body).ID)
	if got = fake.LastRequest(); got.SystemPrompt != "" || got.Permissions != agent.PermissionsAsk || got.Model != "sonnet" {
		t.Fatalf("continue overrides = %+v", got)
	}
	_, body = do(t, http.MethodGet, server.URL+"/v1/sessions/"+started.Context.SessionID, nil)
	if view := decode[session.View](t, body); view.Settings == nil || view.Settings.Tools.Permissions != agent.PermissionsAsk || view.Runs.Count != 3 {
		t.Fatalf("remembered settings: %s", body)
	}
	_, body = do(t, http.MethodGet, server.URL+"/v1/workspaces/"+workspaceID, nil)
	if used := decode[workspace.Info](t, body).Times.LastUsedAt; used == nil || used.IsZero() {
		t.Fatalf("workspace last use: %s", body)
	}

	resp, body = do(t, http.MethodPost, server.URL+"/v1/sessions/"+started.Context.SessionID+"/fork", nil)
	forked := decode[session.View](t, body)
	if resp.StatusCode != http.StatusCreated || forked.Lineage.ForkFrom != started.Context.SessionID || forked.SessionID == started.Context.SessionID || forked.State != session.StateIdle {
		t.Fatalf("fork: %d %s", resp.StatusCode, body)
	}
	if resp, body = do(t, http.MethodPost, server.URL+"/v1/sessions/"+forked.SessionID+"/fork", nil); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(body), "session_not_started") {
		t.Fatalf("fork before the first run: %d %s", resp.StatusCode, body)
	}
	resp, body = continueSession(t, server.URL, forked.SessionID, `{"prompt":"branch"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("continue fork: %d %s", resp.StatusCode, body)
	}
	finish(t, server.URL+"/v1/runs/"+decode[run.Snapshot](t, body).ID)
	if got = fake.LastRequest(); got.SessionMode != agent.SessionFork || got.ForkFrom != started.Context.SessionID || got.NativeForkFrom != started.Context.SessionID ||
		got.SessionID != forked.SessionID || got.Permissions != agent.PermissionsAsk {
		t.Fatalf("fork request = %+v", got)
	}

	// An ask in a workspace returns a session that can be continued.
	resp, body = do(t, http.MethodPost, server.URL+"/v1/ask", strings.NewReader(`{"prompt":"q",`+modelJSON+bypassJSON+`,"workspace_id":"`+workspaceID+`"}`))
	asked := decode[run.Snapshot](t, body)
	if resp.StatusCode != http.StatusOK || asked.Context.SessionID == "" || asked.Result.Text != "echo: q" {
		t.Fatalf("ask in workspace: %d %s", resp.StatusCode, body)
	}
	if resp, body = continueSession(t, server.URL, asked.Context.SessionID, `{"prompt":"follow-up"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("continue ask: %d %s", resp.StatusCode, body)
	}
	finish(t, server.URL+"/v1/runs/"+decode[run.Snapshot](t, body).ID)

	if resp, _ = do(t, http.MethodPost, server.URL+"/v1/sessions/"+testSessionID+"/continue", strings.NewReader(`{"prompt":"x"}`)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("continue unknown session: %d", resp.StatusCode)
	}
}

const testSessionID = "123e4567-e89b-42d3-a456-426614174000"

func TestInteractiveRunOverREST(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{})
	runURL := createWorkspaceRun(t, server.URL, "")

	pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	resp, body := do(t, http.MethodPost, runURL+"/message", strings.NewReader(`{"prompt":"second"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("message: %d %s", resp.StatusCode, body)
	}
	snapshot := pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 2 })
	if snapshot.Result.Text != "echo: second" || snapshot.Status != run.StatusRunning || !snapshot.Mode.Interactive {
		t.Fatalf("after message: %+v", snapshot)
	}
	if resp, _ = do(t, http.MethodPost, runURL+"/message", strings.NewReader(`{"prompt":""}`)); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty message: %d", resp.StatusCode)
	}
	if resp, _ = do(t, http.MethodPost, runURL+"/interrupt", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupt: %d", resp.StatusCode)
	}
	finish(t, runURL)
	if resp, body = do(t, http.MethodPost, runURL+"/message", strings.NewReader(`{"prompt":"late"}`)); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(body), "run_finished") {
		t.Fatalf("message after finish: %d %s", resp.StatusCode, body)
	}
}

func TestAskRunTakesNoMessages(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Blocking(release)})
	workspaceID := newWorkspace(t, server.URL)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	payload := `{"prompt":"hi",` + modelJSON + bypassJSON + `,"workspace_id":"` + workspaceID + `"}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/ask/stream", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	runURL := server.URL + "/v1/runs/" + stream.Header.Get("X-Run-ID")

	resp, body := do(t, http.MethodPost, runURL+"/message", strings.NewReader(`{"prompt":"more"}`))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "run_not_interactive") {
		t.Fatalf("message: %d %s", resp.StatusCode, body)
	}
	// The ask run belongs to its request: disconnecting cancels it.
	cancel()
	if final := pollRun(t, runURL, func(s run.Snapshot) bool { return s.Status.Terminal() }); final.Status != run.StatusCancelled {
		t.Fatalf("after disconnect: %+v", final)
	}
}

func TestPermissionAnswerOverREST(t *testing.T) {
	t.Parallel()
	fake := &agenttest.Agent{Script: agenttest.Permission}
	server := newServer(t, testConfig(t), fake)
	runURL := createWorkspaceRun(t, server.URL, `,"tools":{"permissions":"ask"}`)

	snapshot := pollRun(t, runURL, func(s run.Snapshot) bool { return len(s.Waiting.Permissions) == 1 })
	if fake.LastRequest().Permissions != agent.PermissionsAsk || snapshot.Mode.Permissions != agent.PermissionsAsk {
		t.Fatalf("permissions not passed: %+v", snapshot)
	}
	id := snapshot.Waiting.Permissions[0].ID
	if snapshot.Waiting.Permissions[0].Times.CreatedAt.IsZero() {
		t.Fatalf("permission without creation time: %+v", snapshot.Waiting.Permissions[0])
	}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/permissions/" + id + "/allow", `{}`, http.StatusUnprocessableEntity},
		{"/permissions/" + id + "/allow", `{"scope":"once","provider":[1]}`, http.StatusUnprocessableEntity},
		{"/permissions/unknown/deny", `{"message":"no"}`, http.StatusNotFound},
		{"/permissions/" + id + "/allow", `{"scope":"once","provider":{"name":"codex"}}`, http.StatusUnprocessableEntity},
		{"/permissions/" + id + "/allow", `{"scope":"once","provider":{"name":"fake"}}`, http.StatusAccepted},
	} {
		if resp, body := do(t, http.MethodPost, runURL+tc.path, strings.NewReader(tc.body)); resp.StatusCode != tc.want {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, resp.StatusCode, body)
		}
	}
	turn := pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	if turn.Result == nil || turn.Result.Text != "allow=true message=" {
		t.Fatalf("turn: %+v", turn)
	}
	finish(t, runURL)
	_, events := do(t, http.MethodGet, runURL+"/events", nil)
	if !strings.Contains(string(events), "event: permission_request\n") || !strings.Contains(string(events), `"behavior":"allow"`) {
		t.Fatalf("events:\n%s", events)
	}
}

func TestInteractiveRunOverWebSocket(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Permission})
	runURL := createWorkspaceRun(t, server.URL, `,"tools":{"permissions":"ask"}`)

	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(runURL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read := func(wantType string) map[string]any {
		t.Helper()
		for {
			var frame map[string]any
			if err := wsjson.Read(t.Context(), conn, &frame); err != nil {
				t.Fatalf("waiting for %s: %v", wantType, err)
			}
			if frame["type"] == wantType {
				return frame
			}
		}
	}
	send := func(message map[string]any) {
		t.Helper()
		if err := wsjson.Write(t.Context(), conn, message); err != nil {
			t.Fatal(err)
		}
	}

	request := read(run.EventPermissionRequest)["data"].(map[string]any)
	send(map[string]any{"type": "permission", "permission_id": request["permission_id"], "behavior": "deny", "scope": "once", "message": "not now"})
	if turn := read(run.EventTurn)["data"].(map[string]any); turn["result"].(map[string]any)["text"] != "allow=false message=not now" {
		t.Fatalf("turn 1 = %v", turn)
	}
	send(map[string]any{"type": "permission", "permission_id": "nope", "behavior": "allow", "scope": "once"})
	if failure := read("control_error")["data"].(map[string]any); failure["code"] != "permission_not_pending" {
		t.Fatalf("control_error = %v", failure)
	}
	send(map[string]any{"type": "message", "prompt": "again"})
	second := read(run.EventPermissionRequest)["data"].(map[string]any)
	send(map[string]any{"type": "permission", "permission_id": second["permission_id"], "behavior": "allow", "scope": "once"})
	read(run.EventTurn)
	send(map[string]any{"type": "close"})
	if done := read(run.EventDone)["data"].(map[string]any); done["progress"].(map[string]any)["turns"] != float64(2) {
		t.Fatalf("done = %v", done)
	}
}

func TestPermissionScopesAndRulesAPI(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Permission})
	runURL := createWorkspaceRun(t, server.URL, `,"tools":{"permissions":"ask"}`)
	_, body := do(t, http.MethodGet, runURL, nil)
	workspaceURL := server.URL + "/v1/workspaces/" + decode[run.Snapshot](t, body).Context.WorkspaceID

	id := pollRun(t, runURL, func(s run.Snapshot) bool { return len(s.Waiting.Permissions) == 1 }).Waiting.Permissions[0].ID
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"scope":"global"}`, http.StatusUnprocessableEntity},
		{`{"scope":"workspace"}`, http.StatusAccepted},
	} {
		if resp, body := do(t, http.MethodPost, runURL+"/permissions/"+id+"/allow", strings.NewReader(tc.body)); resp.StatusCode != tc.want {
			t.Fatalf("%s: %d %s", tc.body, resp.StatusCode, body)
		}
	}
	pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	do(t, http.MethodPost, runURL+"/message", strings.NewReader(`{"prompt":"again"}`))
	if s := pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 2 }); s.Result.Text != "allow=true message=" {
		t.Fatalf("second turn not auto-allowed: %+v", s)
	}

	resp, body := do(t, http.MethodGet, workspaceURL+"/permissions", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"workspace":["file_change"]`) {
		t.Fatalf("rules: %d %s", resp.StatusCode, body)
	}
	if resp, _ = do(t, http.MethodPost, workspaceURL+"/permissions/clear", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
	if _, body = do(t, http.MethodGet, workspaceURL+"/permissions", nil); !strings.Contains(string(body), `"workspace":[]`) {
		t.Fatalf("rules after clear: %s", body)
	}
	if resp, _ = do(t, http.MethodGet, server.URL+"/v1/workspaces/"+strings.Repeat("b", 32)+"/permissions", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown workspace rules: %d", resp.StatusCode)
	}
	finish(t, runURL)
}

func TestControlEndpoints(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Hook})
	runURL := createWorkspaceRun(t, server.URL, "")

	pollRun(t, runURL, func(s run.Snapshot) bool { return len(s.Waiting.Controls) == 1 })
	for _, tc := range []struct {
		path, body, contains string
		want                 int
	}{
		{"/native/control", `{"request":{"subtype":"get_context_usage"}}`, `"echo"`, http.StatusOK},
		{"/native/control", `{"request":{"subtype":"fail"}}`, "control_rejected", http.StatusUnprocessableEntity},
		{"/native/control", `{"request":[1]}`, "invalid_request", http.StatusUnprocessableEntity},
		{"/native/controls/hook-1/answer", `{"response":{"a":1},"error":"x"}`, "invalid_request", http.StatusUnprocessableEntity},
		{"/native/controls/unknown/answer", `{"response":{}}`, "control_not_pending", http.StatusNotFound},
		{"/native/controls/hook-1/answer", `{"response":{"continue":true}}`, `"run_id"`, http.StatusAccepted},
	} {
		resp, body := do(t, http.MethodPost, runURL+tc.path, strings.NewReader(tc.body))
		if resp.StatusCode != tc.want || !strings.Contains(string(body), tc.contains) {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, resp.StatusCode, body)
		}
	}
	if final := finish(t, runURL); final.Result.Text != `id=hook-1 response={"continue":true} error=` {
		t.Fatalf("final: %+v", final)
	}
}

func TestNativeControlOverWebSocket(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Hook})
	runURL := createWorkspaceRun(t, server.URL, "")

	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(runURL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, message := range []map[string]any{
		{"type": "control", "id": "c1", "request": map[string]any{"subtype": "mcp_status"}},
		{"type": "control", "id": "c2", "request": map[string]any{"subtype": "fail"}},
	} {
		if err := wsjson.Write(t.Context(), conn, message); err != nil {
			t.Fatal(err)
		}
	}
	replies := map[string]map[string]any{}
	for len(replies) < 2 {
		var frame map[string]any
		if err := wsjson.Read(t.Context(), conn, &frame); err != nil {
			t.Fatal(err)
		}
		if id, ok := frame["id"].(string); ok {
			replies[id] = frame
		}
	}
	if replies["c1"]["type"] != "control_result" || replies["c2"]["type"] != "control_error" {
		t.Fatalf("replies = %v", replies)
	}
	if err := wsjson.Write(t.Context(), conn, map[string]any{"type": "control_response", "control_id": "hook-1", "error": "denied"}); err != nil {
		t.Fatal(err)
	}
	pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Turns == 1 })
	if final := finish(t, runURL); final.Result.Text != "id=hook-1 response= error=denied" {
		t.Fatalf("final result: %q", final.Result.Text)
	}
}

func TestPartialEventsInSSE(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Streaming})

	resp, body := do(t, http.MethodPost, server.URL+"/v1/ask/stream", strings.NewReader(`{"prompt":"hi",`+modelJSON+`}`))
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "event: partial") {
		t.Fatalf("partial events without ?partial=true: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPost, server.URL+"/v1/ask/stream?partial=true", strings.NewReader(`{"prompt":"hi",`+modelJSON+`}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ask stream: %d %s", resp.StatusCode, body)
	}
	// Partial events never carry an id: Last-Event-ID always names a stored event.
	for block := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n\n") {
		if strings.Contains(block, "event: partial") && strings.Contains(block, "id: ") {
			t.Fatalf("partial event with id: %q", block)
		}
	}
	_, stored := do(t, http.MethodGet, server.URL+"/v1/runs/"+resp.Header.Get("X-Run-ID")+"/events?partial=true", nil)
	if strings.Contains(string(stored), "event: partial") {
		t.Fatalf("partial events were stored:\n%s", stored)
	}
}

func TestLinkedWorkspaceAndExtraDirectories(t *testing.T) {
	t.Parallel()
	projects := t.TempDir()
	app, lib := filepath.Join(projects, "app"), filepath.Join(projects, "lib")
	for _, dir := range []string{app, lib} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(t)
	cfg.AllowedRoots = []string{projects}
	fake := &agenttest.Agent{Script: agenttest.Echo}
	server := newServer(t, cfg, fake)

	for _, tc := range []struct {
		body string
		want int
		code string
	}{
		{`{"path":"` + t.TempDir() + `"}`, http.StatusForbidden, "path_not_allowed"},
		{`{"path":"relative"}`, http.StatusForbidden, "path_not_allowed"},
		{`{"path":1}`, http.StatusUnprocessableEntity, "invalid_request"},
	} {
		if resp, body := do(t, http.MethodPost, server.URL+"/v1/workspaces/attach", strings.NewReader(tc.body)); resp.StatusCode != tc.want || !strings.Contains(string(body), tc.code) {
			t.Fatalf("%s: %d %s", tc.body, resp.StatusCode, body)
		}
	}
	resp, body := do(t, http.MethodPost, server.URL+"/v1/workspaces/attach", strings.NewReader(`{"path":"`+app+`"}`))
	attached := decode[workspace.Info](t, body)
	if resp.StatusCode != http.StatusCreated || attached.Storage != "attachment" || attached.Attachment == nil || attached.Attachment.State != "ready" {
		t.Fatalf("link: %d %s", resp.StatusCode, body)
	}
	if _, body = do(t, http.MethodGet, server.URL+"/v1/workspaces", nil); !strings.Contains(string(body), attached.ID) {
		t.Fatalf("list: %s", body)
	}

	outside := `{"prompt":"hi",` + modelJSON + `,"workspace_id":"` + attached.ID + `","tools":{"permissions":"bypass","add_dirs":["/etc"]}}`
	if resp, body := do(t, http.MethodPost, server.URL+"/v1/runs/start", strings.NewReader(outside)); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "tools.add_dirs") {
		t.Fatalf("outside add_dir: %d %s", resp.StatusCode, body)
	}
	started := startRun(t, server.URL, attached.ID, `,"tools":{"permissions":"bypass","add_dirs":["`+lib+`"]}`)
	finish(t, server.URL+"/v1/runs/"+started.ID)
	got := fake.LastRequest()
	realApp, _ := filepath.EvalSymlinks(app)
	realLib, _ := filepath.EvalSymlinks(lib)
	if got.WorkspaceDir != realApp || len(got.AddDirs) != 1 || got.AddDirs[0] != realLib {
		t.Fatalf("request dir=%q add_dirs=%v", got.WorkspaceDir, got.AddDirs)
	}

	if resp, _ := do(t, http.MethodPost, server.URL+"/v1/workspaces/"+attached.ID+"/delete", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete an attached directory: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPost, server.URL+"/v1/workspaces/"+attached.ID+"/detach", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("detach: %d", resp.StatusCode)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("project removed on detach: %v", err)
	}
}

func TestAttachmentDisabledWithoutRoots(t *testing.T) {
	t.Parallel()
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: agenttest.Echo})
	resp, body := do(t, http.MethodPost, server.URL+"/v1/workspaces/attach", strings.NewReader(`{"path":"`+t.TempDir()+`"}`))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "attachment_disabled") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// TestStalledStreamReaderIsDisconnected: an SSE reader that stops reading must not hold its
// connection (and the run) until TCP gives up.
func TestStalledStreamReaderIsDisconnected(t *testing.T) {
	defer httpapi.SetStreamWriteTimeout(200 * time.Millisecond)()
	payload := `{"pad":"` + strings.Repeat("x", 64<<10) + `"}`
	flood := func(ctx context.Context, s *agenttest.Session) error {
		if err := s.StartTurn(); err != nil {
			return err
		}
		for range 400 { // ~26 MB, more than the socket buffers hold
			if err := s.Emit("native", payload); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	server := newServer(t, testConfig(t), &agenttest.Agent{Script: flood})
	runURL := createWorkspaceRun(t, server.URL, "")
	pollRun(t, runURL, func(s run.Snapshot) bool { return s.Progress.Events >= 400 })

	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	path := strings.TrimPrefix(runURL, server.URL) + "/events"
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, address); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // Stalled: the server's write blocks and times out.

	// The server must have closed the connection: draining what the buffers hold ends in EOF.
	// Without the write timeout the server keeps streaming while the run lives.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("stream still open after the reader stalled: %v", err)
	}
	do(t, http.MethodPost, runURL+"/cancel", nil)
}
