package httpapi

import (
	"net/http"
	"path"
	"reflect"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/run"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

const apiDescription = "One gateway process serves exactly one agent CLI. Every route except `/health`, `/ready`,\n" +
	"`/openapi.yaml` and `/docs` requires `Authorization: Bearer <token>` when a token is configured.\n\n" +
	"**Conversations.** `POST /v1/runs/start` creates a session in a workspace and starts its first\n" +
	"run; `POST /v1/sessions/{session_id}/continue` starts the next one with only a `prompt`, reusing\n" +
	"the session's settings. A run is one live agent process: it accepts further messages until it is\n" +
	"closed, cancelled or idle for `NATIVE_AGENT_RUN_IDLE_SECONDS`; a message sent during a turn is\n" +
	"queued and starts the next turn. `POST /v1/ask` (or `/v1/ask/stream`) answers a single prompt.\n\n" +
	"**Settings** are grouped: `model {id, effort}`, `tools {permissions, add_dirs}`,\n" +
	"`instructions {system_prompt}` and `output {schema}`. Omitted optional settings keep the agent's\n" +
	"native behavior.\n\n" +
	"**Provider sections.** Every field outside a `provider` object is gateway data or data every agent\n" +
	"supplies with the same meaning, and is always present. What only one agent has lives in a\n" +
	"`provider` object, in requests and responses alike: `{\"name\": \"<agent>\", ...its own fields}`;\n" +
	"responses add `native`, the unmodified native document. `name` selects the schema variant; a\n" +
	"request section naming another agent is rejected (`provider_mismatch`). Example run options:\n" +
	"`\"provider\": {\"name\": \"claude\", \"max_turns\": 8}`.\n\n" +
	"**Paths** name the action: every change is a `POST` to a command path such as\n" +
	"`/v1/workspaces/{workspace_id}/delete` or `/v1/runs/{run_id}/cancel`; reads are `GET`.\n\n" +
	"**Streaming.** A run records a sequence-numbered event log. Read it as Server-Sent Events\n" +
	"(`GET /v1/runs/{run_id}/events`, resumable with `Last-Event-ID`) or over a WebSocket\n" +
	"(`GET /v1/runs/{run_id}/ws`, which also accepts control messages). Add `?partial=true` to also\n" +
	"receive live token deltas. Event types:\n\n" +
	"| type | data |\n|---|---|\n" +
	"| `agent` | native CLI event, unmodified |\n" +
	"| `turn` | TurnInfo: end of a turn |\n" +
	"| `permission_request` | RunPermission (runs with `tools.permissions: ask`) |\n" +
	"| `permission_resolved` | PermissionResolution (`auto: true` when a session/workspace rule answered) |\n" +
	"| `control_request` | RunControl: native request from the agent the client must answer |\n" +
	"| `control_resolved` | ControlResolution `{control_id}` |\n" +
	"| `partial` | native incremental output, only with `?partial=true`; **not stored**: no `id`/`seq`, only live readers receive it |\n" +
	"| `diagnostic` | bounded, redacted adapter diagnostics explicitly requested for the run |\n" +
	"| `done` | Snapshot (terminal, success) |\n" +
	"| `error` | Snapshot (terminal; `error.code`: `cancelled`, `timeout`, `agent_run_failed`, `agent_start_failed`) |\n\n" +
	"**Permissions.** Every workspace request explicitly selects `tools.permissions: bypass` (all tools\n" +
	"without asking) or `tools.permissions: ask` (each tool use waits for an answer)\n" +
	"(`POST /v1/runs/{run_id}/permissions/{permission_id}/allow` or `/deny`, or the WebSocket `permission`\n" +
	"control); waiting counts toward the turn timeout. Each request has a `kind` (`command`,\n" +
	"`file_change`, `mcp`, `other`) and a `summary`; the native tool is in its `provider` section. An\n" +
	"`allow` may carry `scope: session` or `scope: workspace`: later requests of the same kind in that\n" +
	"session/workspace are then answered automatically. Rules live in gateway memory only (not in agent settings or the\n" +
	"workspace); list them with `GET /v1/workspaces/{workspace_id}/permissions` and revoke them with\n" +
	"`POST /v1/workspaces/{workspace_id}/permissions/clear`.\n\n" +
	"**Native control.** `POST /v1/runs/{run_id}/native/control` forwards any native control request\n" +
	"(Claude: `set_model`, `set_permission_mode`, `get_context_usage`, `mcp_status`, …) and\n" +
	"returns the agent's response. Control requests sent by the agent (Claude: `hook_callback`,\n" +
	"`mcp_message` after an `initialize` that registered them) arrive as `control_request`\n" +
	"events and are answered with `POST /v1/runs/{run_id}/native/controls/{control_id}/answer`.\n\n" +
	"**Responses.** A successful response is the resource itself, without an envelope. Lists are\n" +
	"`{items, next_cursor}` with up to 100 items per page; pass `next_cursor` as `cursor` with the same\n" +
	"filters to continue. Errors are RFC 9457 problem details (`application/problem+json`):\n" +
	"`{title, status, detail, code, request_id, errors}`, where `code` is stable and `errors` lists\n" +
	"invalid fields. Every response has an `X-Request-ID` header: the client's own value (1-128 characters\n" +
	"of `A-Za-z0-9._:/+=-`) or one created by the gateway. It is logged with every record of the request\n" +
	"and stored as the `trace.request_id` of a run the request started.\n"

const wsDescription = "Server → client: Event objects as JSON text frames, then a normal closure after the terminal\n" +
	"event. Client → server control messages:\n\n" +
	"- `{\"type\":\"message\",\"prompt\":\"...\"}`\n" +
	"- `{\"type\":\"interrupt\"}`\n" +
	"- `{\"type\":\"permission\",\"permission_id\":\"...\",\"behavior\":\"allow\"|\"deny\",\"scope\":\"once\"|\"session\"|\"workspace\",\"message\":\"...\",\"provider\":{\"name\":\"<agent>\",...}}`\n" +
	"- `{\"type\":\"control\",\"id\":\"<your id>\",\"request\":{\"subtype\":\"...\"}}` → `{\"type\":\"control_result\",\"id\":...,\"data\":{\"response\":...}}`\n" +
	"- `{\"type\":\"control_response\",\"control_id\":\"...\",\"response\":{...}}` or `\"error\":\"...\"`\n" +
	"- `{\"type\":\"close\"}`\n" +
	"- `{\"type\":\"cancel\"}`\n\n" +
	"A rejected control is answered with `{\"type\":\"control_error\",\"data\":{\"control\",\"code\",\"message\"}}`\n" +
	"(codes as in the REST endpoints). Browsers must be same-origin.\n"

// newAPI registers every operation on mux. The OpenAPI document is generated from the Go
// types of the operations; it is never written by hand. A registration's Summary is one sentence
// about the operation; openapi_names.go gives each operation its group and short name.
// The gateway sends an empty list rather than null, so no array schema is nullable.
func init() { huma.DefaultArrayNullable = false } //nolint:gochecknoinits // Huma reads this global when schemas are generated.

func (s *Server) newAPI(mux *http.ServeMux) huma.API {
	cfg := huma.DefaultConfig(buildinfo.Name, buildinfo.Get().Version)
	// The spec and the docs page are served by this package (pinned Scalar, strict CSP).
	cfg.OpenAPIPath, cfg.DocsPath, cfg.SchemasPath = "", "", ""
	cfg.CreateHooks = nil // No "$schema" links in responses.
	cfg.Transformers = []huma.Transformer{withRequestID}
	cfg.Formats = map[string]huma.Format{"application/json": huma.DefaultJSONFormat, "json": huma.DefaultJSONFormat}
	cfg.Info.Description = apiDescription
	cfg.Components.Schemas = huma.NewMapRegistry("#/components/schemas/", schemaName)
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{"bearer": {
		Type: "http", Scheme: "bearer",
		Description: "Token created with `" + buildinfo.Name + " new-token`; the gateway stores only its SHA-256.",
	}}
	cfg.Security = []map[string][]string{{"bearer": {}}}
	api := humago.New(mux, cfg)
	providerComponents(api.OpenAPI().Components.Schemas, s.providerSections())

	limit := s.cfg.Limits.MaxBodyBytes
	snapshot := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[run.Snapshot](), true, "")
	eventStream := &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Description: "text/event-stream"}}
	binary := &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}
	public := []map[string][]string{{}}

	// System and agent.
	huma.Register(api, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: healthPath,
		Summary: "Liveness probe", Security: public,
	}, s.health)
	huma.Register(api, huma.Operation{
		OperationID: "readiness", Method: http.MethodGet, Path: readyPath,
		Summary: "Readiness and component status", Security: public,
		Responses: map[string]*huma.Response{"503": {Description: "One or more required components are unavailable."}},
	}, s.readiness)
	huma.Register(api, huma.Operation{
		OperationID: "getAgent", Method: http.MethodGet, Path: "/v1/agent",
		Summary: "The served agent, its CLI version and this gateway's build",
	}, s.agentInfo)
	huma.Register(api, huma.Operation{
		OperationID: "getAgentDiagnostics", Method: http.MethodGet, Path: "/v1/agent/diagnostics",
		Summary: "Bounded read-only diagnostics from the native CLI", Errors: []int{501, 502},
	}, s.agentDiagnostics)
	huma.Register(api, huma.Operation{
		OperationID: "getAgentUsage", Method: http.MethodGet, Path: "/v1/agent/usage",
		Summary: "Subscription usage (agents that support it)", Errors: []int{401, 403, 409, 501, 502},
	}, s.agentUsage)
	huma.Register(api, huma.Operation{
		OperationID: "listAgentModels", Method: http.MethodGet, Path: "/v1/agent/models",
		Summary: "Models reported by the native CLI, with the reasoning efforts each accepts", Errors: []int{401, 409, 501, 502},
	}, s.agentModels)
	s.registerAuth(api)

	// Ask.
	askDescription := "Answers one prompt in a single-turn run tied to this request: if the client disconnects the run " +
		"is cancelled. With `workspace_id` the agent works natively in the workspace and the answer's `session_id` can be " +
		"continued; without it the model answers without tools in a private, throwaway directory and nothing is kept."
	huma.Register(api, huma.Operation{
		OperationID: "ask", Method: http.MethodPost, Path: "/v1/ask",
		Summary: "Ask and wait for the answer", Description: askDescription + " `tools.permissions: ask` needs `/v1/ask/stream`.",
		MaxBodyBytes: limit, Errors: []int{404, 409, 422, 429, 502, 504},
		Responses: map[string]*huma.Response{"200": {
			Description: "Final run snapshot.",
			Headers:     map[string]*huma.Param{"X-Run-ID": {Schema: &huma.Schema{Type: huma.TypeString}}},
			Content:     map[string]*huma.MediaType{"application/json": {Schema: snapshot}},
		}},
	}, s.ask)
	huma.Register(api, huma.Operation{
		OperationID: "askStream", Method: http.MethodPost, Path: "/v1/ask/stream",
		Summary: "Ask and stream the run's events", Description: askDescription + " The `X-Run-ID` header names the run, " +
			"e.g. to answer permission requests.",
		MaxBodyBytes: limit, Errors: []int{404, 409, 422, 429},
		Responses: map[string]*huma.Response{"200": {
			Description: "Event stream (Server-Sent Events, as GET /v1/runs/{run_id}/events).",
			Headers:     map[string]*huma.Param{"X-Run-ID": {Schema: &huma.Schema{Type: huma.TypeString}}},
			Content:     map[string]*huma.MediaType{"text/event-stream": eventStream},
		}},
	}, s.askStream)

	// Runs.
	huma.Register(api, huma.Operation{
		OperationID: "startRun", Method: http.MethodPost, Path: "/v1/runs/start",
		Summary: "Start a new conversation in a workspace",
		Description: "Creates a session (its `session_id` is in the response) and starts its first run. The run keeps going " +
			"when the client disconnects, accepts further messages and ends when closed or idle. Continue later with " +
			"`POST /v1/sessions/{session_id}/continue`.",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422, 429},
	}, s.createRun)
	huma.Register(api, huma.Operation{
		OperationID: "listRuns", Method: http.MethodGet, Path: "/v1/runs",
		Summary: "Filter and page runs that are running or within retention, newest first", Errors: []int{422},
	}, s.listRuns)
	huma.Register(api, huma.Operation{
		OperationID: "getRun", Method: http.MethodGet, Path: "/v1/runs/{run_id}",
		Summary: "Run status and result", Errors: []int{404},
	}, s.getRun)
	huma.Register(api, huma.Operation{
		OperationID: "streamRunEvents", Method: http.MethodGet, Path: "/v1/runs/{run_id}/events",
		Summary: "Run events as Server-Sent Events",
		Description: "Each event is `id: <seq>`, `event: <type>`, `data: <json>` (partial events have no id). A `: ping` comment " +
			"is sent every 15 s. The stream ends after the terminal `done` or `error` event.",
		Errors:    []int{400, 404},
		Responses: map[string]*huma.Response{"200": {Description: "Event stream.", Content: map[string]*huma.MediaType{"text/event-stream": eventStream}}},
	}, s.runEvents)
	huma.Register(api, huma.Operation{
		OperationID: "runWebSocket", Method: http.MethodGet, Path: "/v1/runs/{run_id}/ws",
		Summary: "Run events and control over a WebSocket", Description: wsDescription, Errors: []int{400, 404},
		Responses: map[string]*huma.Response{"101": {Description: "Switching to the WebSocket protocol."}},
	}, s.runWebSocket)
	huma.Register(api, huma.Operation{
		OperationID: "sendMessage", Method: http.MethodPost, Path: "/v1/runs/{run_id}/message",
		Summary: "Send a message to the conversation", Description: "Queued if a turn is in progress; it then starts the next turn.",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422},
	}, s.sendMessage)
	runAction := func(id, path, summary, description string, handler func(*run.Run) error) {
		huma.Register(api, huma.Operation{
			OperationID: id, Method: http.MethodPost, Path: "/v1/runs/{run_id}/" + path,
			Summary: summary, Description: description, DefaultStatus: http.StatusAccepted, Errors: []int{404, 409},
		},
			s.runAction(path, handler))
	}
	runAction("interruptRun", "interrupt", "Stop the current turn",
		"The turn ends with an error result; the run then waits for the next message.", (*run.Run).Interrupt)
	runAction("closeRun", "close", "Finish the run after the current turn", "", (*run.Run).CloseInput)
	runAction("cancelRun", "cancel", "Cancel a run", "Kills the agent process; the run ends with status `cancelled`.",
		func(target *run.Run) error { target.Cancel(run.ErrCancelled); return nil })
	huma.Register(api, huma.Operation{
		OperationID: "allowPermission", Method: http.MethodPost, Path: "/v1/runs/{run_id}/permissions/{permission_id}/allow", Summary: "Allow a pending tool use",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422},
	}, s.allowPermission)
	huma.Register(api, huma.Operation{
		OperationID: "denyPermission", Method: http.MethodPost, Path: "/v1/runs/{run_id}/permissions/{permission_id}/deny", Summary: "Deny a pending tool use",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422},
	}, s.denyPermission)
	huma.Register(api, huma.Operation{
		OperationID: "sendNativeControl", Method: http.MethodPost, Path: "/v1/runs/{run_id}/native/control",
		Summary: "Send a native control request to the agent", Description: "The agent's response is returned as-is. See the agent's support matrix for subtypes.",
		MaxBodyBytes: limit, Errors: []int{400, 404, 409, 422, 501, 504},
	}, s.sendControl)
	huma.Register(api, huma.Operation{
		OperationID: "answerNativeControl", Method: http.MethodPost, Path: "/v1/runs/{run_id}/native/controls/{control_id}/answer", Summary: "Answer a control request sent by the agent",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422},
	}, s.respondControl)

	// Sessions.
	huma.Register(api, huma.Operation{
		OperationID: "listSessions", Method: http.MethodGet, Path: "/v1/sessions", Summary: "Filter and page gateway session metadata", Errors: []int{422},
	}, s.listSessions)
	huma.Register(api, huma.Operation{
		OperationID: "getSession", Method: http.MethodGet, Path: "/v1/sessions/{session_id}", Summary: "Get gateway session metadata and native transcript ownership", Errors: []int{404},
	}, s.getSession)
	huma.Register(api, huma.Operation{
		OperationID: "continueSession", Method: http.MethodPost, Path: "/v1/sessions/{session_id}/continue", Summary: "Continue a conversation",
		Description: "Starts the session's next run. Only `prompt` is required: the session's settings are reused; a " +
			"supplied field replaces the remembered one (an `options` object replaces the previous one as a whole).",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: limit, Errors: []int{404, 409, 422, 429},
	}, s.continueSession)
	huma.Register(api, huma.Operation{
		OperationID: "forkSession", Method: http.MethodPost, Path: "/v1/sessions/{session_id}/fork", Summary: "Fork a conversation into a new session",
		Description:   "Returns the new session with the parent's settings. Its first continue forks the native conversation.",
		DefaultStatus: http.StatusCreated, Errors: []int{404, 409},
	}, s.forkSession)
	huma.Register(api, huma.Operation{
		OperationID: "deleteSession", Method: http.MethodPost, Path: "/v1/sessions/{session_id}/delete", Summary: "Delete gateway session metadata",
		Description:   "The native CLI keeps its own transcript; the gateway never deletes it. An active session returns 409.",
		DefaultStatus: http.StatusNoContent, Errors: []int{404, 409},
	}, s.deleteSession)

	// Workspaces.
	huma.Register(api, huma.Operation{
		OperationID: "listWorkspaces", Method: http.MethodGet, Path: "/v1/workspaces",
		Summary: "List managed and attached workspaces", Errors: []int{422},
	}, s.listWorkspaces)
	huma.Register(api, huma.Operation{
		OperationID: "createWorkspace", Method: http.MethodPost, Path: "/v1/workspaces/create",
		Summary: "Create an empty managed workspace", DefaultStatus: http.StatusCreated,
	}, s.createWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "attachWorkspace", Method: http.MethodPost, Path: "/v1/workspaces/attach",
		Summary:       "Attach an existing directory as a workspace",
		Description:   "The directory must be inside `NATIVE_AGENT_ALLOWED_ROOTS`. Attaching the same directory again returns the existing workspace.",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, Errors: []int{403, 422},
	}, s.attachWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "getWorkspace", Method: http.MethodGet, Path: "/v1/workspaces/{workspace_id}", Summary: "Describe a workspace", Errors: []int{404},
	}, s.getWorkspace)
	huma.Register(api, huma.Operation{
		OperationID: "deleteWorkspace", Method: http.MethodPost, Path: "/v1/workspaces/{workspace_id}/delete", Summary: "Delete a managed workspace and its files",
		Description:   "Attached directories are detached instead (409 `wrong_workspace_storage`).",
		DefaultStatus: http.StatusNoContent, Errors: []int{404, 409},
	}, s.removeWorkspace("managed", "deleted"))
	huma.Register(api, huma.Operation{
		OperationID: "detachWorkspace", Method: http.MethodPost, Path: "/v1/workspaces/{workspace_id}/detach", Summary: "Detach an attached directory; its files stay untouched",
		DefaultStatus: http.StatusNoContent, Errors: []int{404, 409},
	}, s.removeWorkspace("attachment", "detached"))
	huma.Register(api, huma.Operation{
		OperationID: "listPermissionRules", Method: http.MethodGet, Path: "/v1/workspaces/{workspace_id}/permissions", Summary: "Tools allowed for the workspace or its sessions", Errors: []int{404},
	}, s.listPermissionRules)
	huma.Register(api, huma.Operation{
		OperationID: "clearPermissionRules", Method: http.MethodPost, Path: "/v1/workspaces/{workspace_id}/permissions/clear", Summary: "Revoke all session and workspace rules",
		DefaultStatus: http.StatusNoContent, Errors: []int{404},
	}, s.clearPermissionRules)

	// Files.
	files := "/v1/workspaces/{workspace_id}/files"
	huma.Register(api, huma.Operation{
		OperationID: "listFiles", Method: http.MethodGet, Path: files, Summary: "Page files and directories (symbolic links are skipped)", Errors: []int{404, 422},
	}, s.listFiles)
	huma.Register(api, huma.Operation{
		OperationID: "downloadFile", Method: http.MethodGet, Path: files + "/download/{path...}", Summary: "Download a file (supports Range and conditional requests)", Errors: []int{404, 422},
		Responses: map[string]*huma.Response{
			"200": {Description: "File content.", Content: map[string]*huma.MediaType{"application/octet-stream": binary}},
			"206": {Description: "Requested range.", Content: map[string]*huma.MediaType{"application/octet-stream": binary}},
		},
	}, s.downloadFile)
	huma.Register(api, huma.Operation{
		OperationID: "uploadFile", Method: http.MethodPost, Path: files + "/upload/{path...}", Summary: "Upload a file (streamed, atomic replace)", Description: "The body is stored as-is; Content-Type is not interpreted. The parent directory must exist.",
		DefaultStatus: http.StatusCreated, Errors: []int{404, 413, 422},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": binary}},
	}, s.uploadFile)
	huma.Register(api, huma.Operation{
		OperationID: "deleteFile", Method: http.MethodPost, Path: files + "/delete", Summary: "Delete a file or directory tree",
		DefaultStatus: http.StatusNoContent, MaxBodyBytes: 8 << 10, Errors: []int{404, 422},
	}, s.deleteFile)
	huma.Register(api, huma.Operation{
		OperationID: "makeDirectory", Method: http.MethodPost, Path: files + "/mkdir", Summary: "Create a directory",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, Errors: []int{404, 409, 422},
	}, s.makeDirectory)
	huma.Register(api, huma.Operation{
		OperationID: "copyFile", Method: http.MethodPost, Path: files + "/copy", Summary: "Copy a file or directory tree",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, Errors: []int{404, 409, 413, 422},
	}, s.transfer("copied", s.workspaces.Copy))
	huma.Register(api, huma.Operation{
		OperationID: "moveFile", Method: http.MethodPost, Path: files + "/move", Summary: "Move or rename a file or directory tree",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 8 << 10, Errors: []int{404, 409, 422},
	}, s.transfer("moved", s.workspaces.Move))

	// The {path...} wildcard is ServeMux syntax; OpenAPI names it {path}.
	for key, item := range api.OpenAPI().Paths {
		if fixed := strings.ReplaceAll(key, "...}", "}"); fixed != key {
			delete(api.OpenAPI().Paths, key)
			api.OpenAPI().Paths[fixed] = item
		}
	}
	nameOperations(api.OpenAPI())
	s.documentValues(api.OpenAPI().Components.Schemas)
	documentExamples(api.OpenAPI())

	return api
}

// providerSections are the provider section variants of the document: every adapter's, so a
// section written for another agent passes validation and is rejected as provider_mismatch. A
// served agent without a builtin entry (the test fake) is added with its own sections, if any.
func (s *Server) providerSections() map[string]map[agent.Section]reflect.Type {
	sections := builtin.Sections()
	if s.agent != nil {
		if _, ok := sections[s.agent.Name()]; !ok {
			own := map[agent.Section]reflect.Type{}
			if describer, ok := s.agent.(agent.SectionDescriber); ok {
				own = describer.ProviderSections()
			}
			sections[s.agent.Name()] = own
		}
	}

	return sections
}

// OpenAPI returns the generated OpenAPI 3.1 document (YAML) without starting a server.
func OpenAPI() ([]byte, error) {
	s := &Server{cfg: config.Config{Limits: config.Limits{MaxBodyBytes: 1 << 20}}}
	return s.newAPI(http.NewServeMux()).OpenAPI().YAML()
}

// schemaName prefixes types from other packages with the package name (RunSnapshot,
// WorkspaceInfo, VersionInfo) so equally named types never collide.
func schemaName(t reflect.Type, hint string) string {
	// list[T] is named after its element: WorkspaceInfoList, RunSnapshotList.
	if t.Kind() == reflect.Struct && strings.HasPrefix(t.Name(), "list[") {
		element := t.Field(0).Type.Elem()
		return schemaName(element, element.Name()) + "List"
	}
	name := huma.DefaultSchemaNamer(t, hint)
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	pkg := path.Base(t.PkgPath())
	if t.Name() == "" || t.PkgPath() == "" || pkg == "httpapi" || strings.HasPrefix(strings.ToLower(name), pkg) {
		return name
	}

	return strings.ToUpper(pkg[:1]) + pkg[1:] + name
}
