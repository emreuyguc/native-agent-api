package httpapi

import (
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// operationGroup is one tag of the API document: a folder in API clients and in the reference.
type operationGroup struct {
	tag, description string
	// operations maps each operation ID to its short name, in the order of the document.
	operations [][2]string
}

// operationSections group the tags in the reference (the `x-tagGroups` extension).
var operationSections = []struct {
	name   string
	groups []operationGroup
}{
	{"Agent", []operationGroup{
		{"agent", "The served agent: identity, models, subscription usage and diagnostics.", [][2]string{
			{"getAgent", "Get agent"},
			{"listAgentModels", "List models"},
			{"getAgentUsage", "Get usage"},
			{"getAgentDiagnostics", "Get diagnostics"},
		}},
		{"login", "The native CLI login: start it, finish it in a browser, read its state, log out.", [][2]string{
			{"getLoginStatus", "Login status"},
			{"startLogin", "Start login"},
			{"submitLoginCode", "Submit login code"},
			{"getLogin", "Get login"},
			{"cancelLogin", "Cancel login"},
			{"logout", "Log out"},
		}},
	}},
	{"Runs", []operationGroup{
		{"ask", "One prompt, one answer.", [][2]string{
			{"ask", "Ask"},
			{"askStream", "Ask (stream)"},
		}},
		{"runs", "Interactive runs: start, follow, send messages, stop.", [][2]string{
			{"startRun", "Start run"},
			{"listRuns", "List runs"},
			{"getRun", "Get run"},
			{"streamRunEvents", "Stream events"},
			{"runWebSocket", "WebSocket"},
			{"sendMessage", "Send message"},
			{"interruptRun", "Interrupt run"},
			{"closeRun", "Close run"},
			{"cancelRun", "Cancel run"},
		}},
		{"permissions", "Tool permission requests of a run, and the rules that answer later ones.", [][2]string{
			{"allowPermission", "Allow permission"},
			{"denyPermission", "Deny permission"},
			{"listPermissionRules", "List permission rules"},
			{"clearPermissionRules", "Clear permission rules"},
		}},
		{"native control", "Native control requests between the client and the agent.", [][2]string{
			{"sendNativeControl", "Send control"},
			{"answerNativeControl", "Answer control"},
		}},
		{"sessions", "Conversations that later runs continue.", [][2]string{
			{"listSessions", "List sessions"},
			{"getSession", "Get session"},
			{"continueSession", "Continue session"},
			{"forkSession", "Fork session"},
			{"deleteSession", "Delete session"},
		}},
	}},
	{"Workspaces", []operationGroup{
		{"workspaces", "Directories the agent works in.", [][2]string{
			{"listWorkspaces", "List workspaces"},
			{"createWorkspace", "Create workspace"},
			{"attachWorkspace", "Attach workspace"},
			{"getWorkspace", "Get workspace"},
			{"deleteWorkspace", "Delete workspace"},
			{"detachWorkspace", "Detach workspace"},
		}},
		{"files", "Files of a workspace.", [][2]string{
			{"listFiles", "List files"},
			{"downloadFile", "Download file"},
			{"uploadFile", "Upload file"},
			{"deleteFile", "Delete file"},
			{"makeDirectory", "Create directory"},
			{"copyFile", "Copy file"},
			{"moveFile", "Move file"},
		}},
	}},
	{"System", []operationGroup{
		{"system", "Probes for monitoring.", [][2]string{
			{"health", "Health"},
			{"readiness", "Readiness"},
		}},
	}},
}

// nameOperations gives every operation its group and short name. A registration describes its
// operation in one sentence (Summary); that sentence moves to the front of the description, so
// menus show the short name and the reference keeps the sentence.
func nameOperations(document *huma.OpenAPI) {
	type target struct{ tag, name string }
	targets := map[string]target{}
	var sections []map[string]any
	document.Tags = nil
	for _, section := range operationSections {
		var tags []string
		for _, group := range section.groups {
			tags = append(tags, group.tag)
			document.Tags = append(document.Tags, &huma.Tag{Name: group.tag, Description: group.description})
			for _, operation := range group.operations {
				targets[operation[0]] = target{tag: group.tag, name: operation[1]}
			}
		}
		sections = append(sections, map[string]any{"name": section.name, "tags": tags})
	}
	if document.Extensions == nil {
		document.Extensions = map[string]any{}
	}
	document.Extensions["x-tagGroups"] = sections

	named := 0
	for _, item := range document.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil {
				continue
			}
			to, ok := targets[op.OperationID]
			if !ok {
				panic(fmt.Sprintf("httpapi: operation %s has no group and name", op.OperationID))
			}
			sentence := strings.TrimSuffix(op.Summary, ".") + "."
			op.Description = strings.TrimSpace(sentence + "\n\n" + op.Description)
			op.Summary, op.Tags = to.name, []string{to.tag}
			named++
		}
	}
	if named != len(targets) {
		panic(fmt.Sprintf("httpapi: %d operations are named, %d exist", len(targets), named))
	}
}
