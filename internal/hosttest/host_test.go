//go:build host

// Package hosttest runs the gateway in process against the real native CLI installed on this host,
// logged in with the owner's native profile. It checks what only the real CLI can show: that the
// gateway still drives it, and that the security properties verified by hand still hold after a CLI
// upgrade. It spends a few small model calls.
//
//	make test-host AGENT=claude   # or AGENT=codex
package hosttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"
)

type gateway struct {
	t          *testing.T
	url        string
	agent      string
	workspaces string
	model      map[string]string
}

func start(t *testing.T) *gateway {
	t.Helper()
	name := os.Getenv("NATIVE_AGENT_HOST_AGENT")
	if name == "" {
		t.Skip("set NATIVE_AGENT_HOST_AGENT=claude|codex (make test-host AGENT=...)")
	}
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, example, 0o600); err != nil {
		t.Fatal(err)
	}
	environ, err := config.MergeFile(map[string]string{
		"HOME": os.Getenv("HOME"), "USER": os.Getenv("USER"), "PATH": os.Getenv("PATH"), "TMPDIR": dir,
		"NATIVE_AGENT_AGENT": name, "NATIVE_AGENT_SANDBOX_MODE": "cli", "NATIVE_AGENT_PROFILE_DIR": "native",
		"NATIVE_AGENT_DATA_DIR": filepath.Join(dir, "data"), "NATIVE_AGENT_LISTEN": "127.0.0.1:0",
		"NATIVE_AGENT_ALLOW_UNAUTHENTICATED": "false", "NATIVE_AGENT_LOG_LEVEL": "warn",
	}, envFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(environ)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PrepareDataDir(cfg.WorkDir); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	served, err := builtin.Build(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(t.Context(), cfg, served, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	g := &gateway{t: t, url: server.URL, agent: name, workspaces: cfg.WorkspaceRoot}
	g.model = g.pickModel()

	return g
}

// pickModel uses NATIVE_AGENT_HOST_MODEL, or the last listed model (the CLIs list the small ones
// last), with its lowest effort.
func (g *gateway) pickModel() map[string]string {
	var models struct {
		Items []struct {
			ID      string   `json:"id"`
			Efforts []string `json:"efforts"`
		} `json:"items"`
	}
	g.call(http.MethodGet, "/v1/agent/models", nil, http.StatusOK, &models)
	if len(models.Items) == 0 {
		g.t.Fatal("the CLI reports no models; is it logged in?")
	}
	chosen := models.Items[len(models.Items)-1]
	if id := os.Getenv("NATIVE_AGENT_HOST_MODEL"); id != "" {
		chosen.ID = id
	}
	effort := ""
	if len(chosen.Efforts) > 0 {
		effort = chosen.Efforts[0]
	}

	return map[string]string{"id": chosen.ID, "effort": effort}
}

func (g *gateway) call(method, path string, body any, want int, out any) {
	g.t.Helper()
	var reader io.Reader
	header := http.Header{}
	switch value := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(value)
	default:
		encoded, _ := json.Marshal(value)
		reader, header = bytes.NewReader(encoded), http.Header{"Content-Type": {"application/json"}}
	}
	ctx, cancel := context.WithTimeout(g.t.Context(), 5*time.Minute)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, method, g.url+path, reader)
	request.Header = header
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		g.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		g.t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			g.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}

func (g *gateway) ask(workspace, prompt string) string {
	g.t.Helper()
	body := map[string]any{"prompt": prompt, "model": g.model}
	if workspace != "" {
		body["workspace_id"], body["tools"] = workspace, map[string]string{"permissions": "bypass"}
	}
	var view struct {
		Status string `json:"status"`
		Result *struct {
			Text string `json:"result"`
		} `json:"result"`
	}
	g.call(http.MethodPost, "/v1/ask", body, http.StatusOK, &view)
	if view.Status != "succeeded" || view.Result == nil {
		g.t.Fatalf("ask %q: status %s", prompt, view.Status)
	}

	return view.Result.Text
}

func (g *gateway) workspace() (string, string) {
	g.t.Helper()
	var created struct {
		ID string `json:"workspace_id"`
	}
	g.call(http.MethodPost, "/v1/workspaces/create", map[string]any{}, http.StatusCreated, &created)

	return created.ID, filepath.Join(g.workspaces, created.ID)
}

func (g *gateway) upload(workspace, path, content string) {
	g.t.Helper()
	g.call(http.MethodPost, "/v1/workspaces/"+workspace+"/files/upload/"+path, content, http.StatusCreated, nil)
}

func TestAskWithoutWorkspace(t *testing.T) {
	g := start(t)
	if answer := g.ask("", "Reply with exactly PONG and nothing else."); !strings.Contains(answer, "PONG") {
		t.Fatalf("answer %q", answer)
	}
}

// TestProjectInstructionsLoad checks that native project instructions keep working: the Codex
// workspace must stay an undecided project, which still loads AGENTS.md.
func TestProjectInstructionsLoad(t *testing.T) {
	g := start(t)
	id, _ := g.workspace()
	file := map[string]string{"claude": "CLAUDE.md", "codex": "AGENTS.md"}[g.agent]
	g.upload(id, file, "When the user asks for the codeword, reply with exactly ZEBRA-42 and nothing else.\n")
	if answer := g.ask(id, "What is the codeword?"); !strings.Contains(answer, "ZEBRA-42") {
		t.Fatalf("answer %q: %s was not loaded", answer, file)
	}
}

// TestAgentWrittenConfigNeverStarts covers docs/security.md F-7 and F-8.
func TestAgentWrittenConfigNeverStarts(t *testing.T) {
	g := start(t)
	id, dir := g.workspace()
	marker := filepath.Join(t.TempDir(), "marker")
	switch g.agent {
	case "claude":
		// The file tools run outside the OS sandbox; the gateway deny rules must stop them.
		g.ask(id, `Use the Write tool, not Bash, to create two files: ok.txt containing hi, then .mcp.json containing {"mcpServers":{}}.`)
		if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
			t.Fatal("the agent did not use the Write tool; the check proves nothing")
		}
		if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
			t.Fatal("the agent wrote .mcp.json")
		}
	case "codex":
		// apply_patch can write project config, so Codex must never load it.
		profile := filepath.Join(os.Getenv("HOME"), ".codex", "config.toml")
		g.call(http.MethodPost, "/v1/workspaces/"+id+"/files/mkdir", map[string]string{"path": ".codex", "conflict": "error"}, http.StatusCreated, nil)
		g.upload(id, ".codex/config.toml", fmt.Sprintf("[mcp_servers.marker]\ncommand = \"/usr/bin/touch\"\nargs = [%q]\n", marker))
		g.ask(id, "Reply with exactly OK.")
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("project .codex/config.toml was loaded: its MCP server started")
		}
		if data, _ := os.ReadFile(profile); bytes.Contains(data, []byte(filepath.Base(dir))) {
			t.Fatalf("%s records the workspace as a project", profile)
		}
	}
}
