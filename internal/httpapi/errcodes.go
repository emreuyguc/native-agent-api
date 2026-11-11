package httpapi

import (
	"maps"
	"slices"
	"strings"

	"github.com/emreuyguc/native-agent-api/internal/run"
)

// errorCode is the stable `code` of an error body. Its field is unexported, so a code exists only
// when it is defined below; every defined code is listed in the API document and docs/api.md.
type errorCode struct{ name string }

func (c errorCode) String() string { return c.name }

// errorCodeMeanings holds every code a response can carry, with its meaning.
var errorCodeMeanings = map[string]string{}

func defineCode(name, meaning string) errorCode {
	if _, exists := errorCodeMeanings[name]; exists {
		panic("httpapi: error code " + name + " is defined twice")
	}
	errorCodeMeanings[name] = meaning

	return errorCode{name: name}
}

// Request and transport.
var (
	codeInvalidRequest       = defineCode("invalid_request", "The body, a parameter or a field value is invalid; `errors` lists the fields")
	codeInvalidRequestID     = defineCode("invalid_request_id", "The X-Request-ID header is not 1-128 allowed characters")
	codeInvalidCursor        = defineCode("invalid_cursor", "`cursor` does not identify an item in the filtered list")
	codeInvalidFilter        = defineCode("invalid_filter", "A list filter is invalid")
	codeRequestTooLarge      = defineCode("request_too_large", "The body exceeds NATIVE_AGENT_MAX_BODY_BYTES")
	codeRequestTimeout       = defineCode("request_timeout", "The client sent the body too slowly")
	codeUnsupportedMediaType = defineCode("unsupported_media_type", "The Content-Type is not accepted by the operation")
	codeNotAcceptable        = defineCode("not_acceptable", "No response format matches the Accept header")
	codeMethodNotAllowed     = defineCode("method_not_allowed", "The path does not support this HTTP method")
	codeNotFound             = defineCode("not_found", "No operation exists at this path, or the file does not exist")
	codeInternalError        = defineCode("internal_error", "The gateway failed; the log has the request ID")
)

// Access.
var (
	codeUnauthorized       = defineCode("unauthorized", "A valid bearer token is required")
	codeIPNotAllowed       = defineCode("ip_not_allowed", "The client address is not in NATIVE_AGENT_ALLOWED_IPS")
	codeHostNotAllowed     = defineCode("host_not_allowed", "Without a token the gateway must be addressed by IP address or localhost")
	codeCrossOriginRequest = defineCode("cross_origin_request", "A browser sent a state-changing request from another origin")
)

// Agent and login.
var (
	codeAgentFailed      = defineCode("agent_failed", "The native CLI failed or gave an unusable answer")
	codeUnsupported      = defineCode("unsupported", "The served agent does not support this operation")
	codeForbidden        = defineCode("forbidden", "The native account may not use this feature")
	codeProviderMismatch = defineCode("provider_mismatch", "A `provider` section names another agent than the served one")
	codeLoginRequired    = defineCode("login_required", "The native CLI is not logged in")
	codeLoginExpired     = defineCode("login_expired", "The native login has expired; log in again")
	codeLoginInProgress  = defineCode("login_in_progress", "Another login is starting or pending")
	codeLoginNotFound    = defineCode("login_not_found", "No login has been started")
	codeLoginFinished    = defineCode("login_finished", "The login has already finished")
)

// Runs.
var (
	codeGatewayBusy        = defineCode("gateway_busy", "All NATIVE_AGENT_MAX_CONCURRENT run slots are in use")
	codeRunNotFound        = defineCode("run_not_found", "The run does not exist or its retention has ended")
	codeRunFinished        = defineCode("run_finished", "The run has already finished")
	codeRunNotInteractive  = defineCode("run_not_interactive", "An ask run answers one prompt; continue its session instead")
	codeRunInputClosed     = defineCode("run_input_closed", "The run no longer accepts messages")
	codePermissionNotFound = defineCode("permission_not_pending", "No pending permission request has this ID")
	codeControlNotPending  = defineCode("control_not_pending", "No pending control request has this ID")
	codeControlRejected    = defineCode("control_rejected", "The agent rejected the control request")
	codeControlTimeout     = defineCode("control_timeout", "The agent did not answer the control request in time")
	codeUnsupportedControl = defineCode("unsupported_control", "The WebSocket control message type is unknown")
)

// Sessions.
var (
	codeSessionNotFound          = defineCode("session_not_found", "The session does not exist")
	codeSessionBusy              = defineCode("session_busy", "The session has an active run")
	codeSessionExists            = defineCode("session_exists", "A session with this ID already exists")
	codeSessionNotStarted        = defineCode("session_not_started", "The session has no native conversation to fork yet")
	codeInvalidSession           = defineCode("invalid_session", "The session ID or the session request is invalid")
	codeNativeSessionUnavailable = defineCode("native_session_unavailable", "The session has no native CLI binding")
	codeSessionError             = defineCode("session_error", "A session operation failed in the gateway")
)

// Workspaces and files.
var (
	codeInvalidWorkspace      = defineCode("invalid_workspace", "The workspace ID is invalid")
	codeWorkspaceBusy         = defineCode("workspace_busy", "The workspace has an active session")
	codeWrongWorkspaceStorage = defineCode("wrong_workspace_storage", "The operation does not match the workspace storage; use delete or detach")
	codeAttachmentDisabled    = defineCode("attachment_disabled", "Host-directory attachments need NATIVE_AGENT_ALLOWED_ROOTS")
	codePathNotAllowed        = defineCode("path_not_allowed", "The path is not a directory inside an allowed root")
	codeInvalidPath           = defineCode("invalid_path", "The workspace path is absolute, contains `..`, or is otherwise invalid")
	codeInvalidOperation      = defineCode("invalid_operation", "The file operation is not possible on this path")
	codePathConflict          = defineCode("path_conflict", "The destination exists and `conflict` is `error`")
	codeFileTooLarge          = defineCode("file_too_large", "The file exceeds NATIVE_AGENT_MAX_FILE_BYTES")
	codeTooManyEntries        = defineCode("too_many_entries", "The directory exceeds NATIVE_AGENT_MAX_LIST_ENTRIES")
	codeWorkspaceCreateFailed = defineCode("workspace_create_failed", "The gateway could not create the workspace")
	codeWorkspaceAttachFailed = defineCode("workspace_attach_failed", "The gateway could not attach the host directory")
	codeWorkspaceListFailed   = defineCode("workspace_list_failed", "The gateway could not list the workspaces")
	codeWorkspaceError        = defineCode("workspace_error", "A workspace operation failed in the gateway")
)

// The error of a failed ask is returned as an error body with the run's own code.
var runErrorCodes = func() map[run.ErrorCode]errorCode {
	codes := make(map[run.ErrorCode]errorCode)
	for _, code := range run.ErrorCodes() {
		meaning := code.Meaning()
		codes[code] = defineCode(string(code), "Ask run: "+strings.ToLower(meaning[:1])+meaning[1:])
	}

	return codes
}()

// errorCodes returns every defined code, sorted.
func errorCodes() []string { return slices.Sorted(maps.Keys(errorCodeMeanings)) }
