// Package conformance_test sends the same API payloads to every adapter, each driving a
// deterministic fake of its native CLI, and requires the same observable behavior.
package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/claude"
	"github.com/emreuyguc/native-agent-api/internal/agent/codex"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"
	"github.com/emreuyguc/native-agent-api/internal/run"
)

type adapter struct {
	name string
	// ownOptions are a valid run provider section of this adapter; otherOptions are another agent's.
	ownOptions, otherOptions string
	build                    func(t *testing.T, binary, root string) agent.Agent
}

var adapters = []adapter{
	{
		name:         claude.Name,
		ownOptions:   `{"name":"claude","max_turns":4}`,
		otherOptions: `{"name":"codex","service_tier":"fast"}`,
		build: func(t *testing.T, binary, root string) agent.Agent {
			t.Helper()
			a, err := claude.New(claude.Options{
				Binary: binary, ProfileDir: filepath.Join(root, "profile"), HomeDir: root, User: "test", WorkDir: root,
				TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20, CommandTimeout: 30 * time.Second,
				SandboxMode: agent.SandboxExternal, Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			return a
		},
	},
	{
		name:         codex.Name,
		ownOptions:   `{"name":"codex","service_tier":"fast"}`,
		otherOptions: `{"name":"claude","max_turns":4}`,
		build: func(t *testing.T, binary, root string) agent.Agent {
			t.Helper()
			a, err := codex.New(codex.Options{
				Binary: binary, ProfileDir: filepath.Join(root, "profile"), HomeDir: root, User: "test", WorkDir: root,
				TempDir: root, Path: "/usr/bin:/bin", MaxOutputBytes: 1 << 20, CommandTimeout: 30 * time.Second,
				SandboxMode: agent.SandboxCLI, Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			return a
		},
	},
}

const model = `"model":{"id":"test-model","effort":"low"}`

func TestSamePayloadsOnEveryAdapter(t *testing.T) {
	t.Parallel()
	for _, adapter := range adapters {
		t.Run(adapter.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			api := newGateway(t, adapter.build(t, fakeBinary(t, adapter.name), root), root)

			t.Run("models", func(t *testing.T) {
				var models struct {
					Items []agent.Model `json:"items"`
				}
				api.json(t, http.MethodGet, "/v1/agent/models", "", http.StatusOK, &models)
				if len(models.Items) != 1 || models.Items[0].ID != fakeModel || strings.Join(models.Items[0].Efforts, ",") != "low,high" {
					t.Fatalf("models = %+v", models.Items)
				}
				api.problem(t, "/v1/ask", `{"prompt":"q","model":{"id":"test-model","effort":"turbo"}}`, http.StatusUnprocessableEntity, "invalid_request")
			})

			t.Run("ask without a workspace", func(t *testing.T) {
				var snapshot run.Snapshot
				api.json(t, http.MethodPost, "/v1/ask", `{"prompt":"q",`+model+`}`, http.StatusOK, &snapshot)
				if snapshot.Status != run.StatusSucceeded || snapshot.Result.Text != "stateless|none|q" || snapshot.Context.SessionID != "" {
					t.Fatalf("ask = %+v", snapshot)
				}
			})

			t.Run("provider options", func(t *testing.T) {
				var snapshot run.Snapshot
				api.json(t, http.MethodPost, "/v1/ask", `{"prompt":"q",`+model+`,"provider":`+adapter.ownOptions+`}`, http.StatusOK, &snapshot)
				if snapshot.Result.Text != "stateless|none|q" {
					t.Fatalf("ask with own options = %+v", snapshot)
				}
				api.problem(t, "/v1/ask", `{"prompt":"q",`+model+`,"provider":`+adapter.otherOptions+`}`, http.StatusUnprocessableEntity, "provider_mismatch")
			})

			t.Run("start, continue and fork", func(t *testing.T) {
				workspaceID := api.workspace(t)
				started := api.start(t, `{"prompt":"first",`+model+`,"workspace_id":"`+workspaceID+`","tools":{"permissions":"bypass"}}`)
				if result := api.finish(t, started.ID); result != "new|none|first" {
					t.Fatalf("start = %q", result)
				}
				continued := api.start(t, `{"prompt":"second"}`, "/v1/sessions/"+started.Context.SessionID+"/continue")
				if result := api.finish(t, continued.ID); result != "resume|none|second" {
					t.Fatalf("continue = %q", result)
				}

				var forked struct {
					SessionID string `json:"session_id"`
				}
				api.json(t, http.MethodPost, "/v1/sessions/"+started.Context.SessionID+"/fork", "", http.StatusCreated, &forked)
				branch := api.start(t, `{"prompt":"branch"}`, "/v1/sessions/"+forked.SessionID+"/continue")
				if result := api.finish(t, branch.ID); result != "fork|none|branch" {
					t.Fatalf("fork = %q", result)
				}
				again := api.start(t, `{"prompt":"again"}`, "/v1/sessions/"+forked.SessionID+"/continue")
				if result := api.finish(t, again.ID); result != "resume|none|again" {
					t.Fatalf("continue fork = %q", result)
				}
			})

			t.Run("permissions", func(t *testing.T) {
				workspaceID := api.workspace(t)
				started := api.start(t, `{"prompt":"p1",`+model+`,"workspace_id":"`+workspaceID+`","tools":{"permissions":"ask"}}`)
				id := api.waitPermission(t, started.ID)
				api.json(t, http.MethodPost, "/v1/runs/"+started.ID+"/permissions/"+id+"/allow", `{"scope":"once"}`, http.StatusAccepted, nil)
				if snapshot := api.waitTurns(t, started.ID, 1); snapshot.Result.Text != "new|allow|p1" {
					t.Fatalf("allow = %+v", snapshot.Result)
				}
				api.json(t, http.MethodPost, "/v1/runs/"+started.ID+"/message", `{"prompt":"p2"}`, http.StatusAccepted, nil)
				id = api.waitPermission(t, started.ID)
				api.json(t, http.MethodPost, "/v1/runs/"+started.ID+"/permissions/"+id+"/deny", `{"message":"no"}`, http.StatusAccepted, nil)
				if result := api.finish(t, started.ID); result != "new|deny|p2" {
					t.Fatalf("deny = %q", result)
				}
			})
		})
	}
}

type gateway struct{ url string }

func newGateway(t *testing.T, a agent.Agent, root string) gateway {
	t.Helper()
	cfg := config.Config{
		Network: config.Network{Listen: "127.0.0.1:0"}, Log: config.Log{Format: "json"},
		HomeDir: root, WorkDir: root, WorkspaceRoot: filepath.Join(root, "workspaces"),
		Limits: config.Limits{
			MaxConcurrent: 4, QueryTimeout: 10 * time.Second, CommandTimeout: 10 * time.Second, LoginTimeout: time.Minute,
			MaxBodyBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxListEntries: 100,
			RunRetention: time.Minute, MaxRetainedRuns: 50, MaxRunEvents: 1_000, MaxRunObservations: 20, RunIdle: time.Minute,
		},
	}
	base, cancel := context.WithCancel(context.Background())
	server, err := httpapi.New(base, cfg, a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		listener.Close()
		cancel()
		server.Wait()
	})

	return gateway{url: listener.URL}
}

// json sends a request, checks the status and decodes the body into out (when non-nil).
func (g gateway) json(t *testing.T, method, path, body string, want int, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, g.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if out != nil {
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
	}
}

func (g gateway) problem(t *testing.T, path, body string, want int, code string) {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	g.json(t, http.MethodPost, path, body, want, &problem)
	if problem.Code != code {
		t.Fatalf("%s: code %q, want %q", path, problem.Code, code)
	}
}

func (g gateway) workspace(t *testing.T) string {
	t.Helper()
	var info struct {
		ID string `json:"workspace_id"`
	}
	g.json(t, http.MethodPost, "/v1/workspaces/create", "", http.StatusCreated, &info)
	if info.ID == "" {
		t.Fatal("workspace without id")
	}

	return info.ID
}

// start posts a conversation start (default path) or continue and returns the run.
func (g gateway) start(t *testing.T, body string, path ...string) run.Snapshot {
	t.Helper()
	target := "/v1/runs/start"
	if len(path) > 0 {
		target = path[0]
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, g.url+target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		// The previous run of the session may still be releasing it.
		if resp.StatusCode == http.StatusConflict && strings.Contains(string(data), "session_busy") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST %s: %d %s", target, resp.StatusCode, data)
		}
		var snapshot run.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
}

func (g gateway) poll(t *testing.T, runID string, done func(run.Snapshot) bool) run.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var snapshot run.Snapshot
		g.json(t, http.MethodGet, "/v1/runs/"+runID, "", http.StatusOK, &snapshot)
		if done(snapshot) {
			return snapshot
		}
		if snapshot.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("run %s: %+v (error %+v)", runID, snapshot, snapshot.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (g gateway) waitTurns(t *testing.T, runID string, turns int) run.Snapshot {
	t.Helper()
	return g.poll(t, runID, func(s run.Snapshot) bool { return s.Progress.Turns >= turns && s.Result != nil })
}

func (g gateway) waitPermission(t *testing.T, runID string) string {
	t.Helper()
	return g.poll(t, runID, func(s run.Snapshot) bool { return len(s.Waiting.Permissions) == 1 }).Waiting.Permissions[0].ID
}

// finish waits for the current turn, closes the run and returns the last answer.
func (g gateway) finish(t *testing.T, runID string) string {
	t.Helper()
	turns := g.poll(t, runID, func(s run.Snapshot) bool { return s.Result != nil && len(s.Waiting.Permissions) == 0 }).Progress.Turns
	g.waitTurns(t, runID, turns)
	g.json(t, http.MethodPost, "/v1/runs/"+runID+"/close", "", http.StatusAccepted, nil)
	snapshot := g.poll(t, runID, func(s run.Snapshot) bool { return s.Status.Terminal() })
	if snapshot.Status != run.StatusSucceeded {
		t.Fatalf("run %s ended %s: %+v", runID, snapshot.Status, snapshot.Error)
	}

	return snapshot.Result.Text
}
