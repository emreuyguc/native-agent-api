package httpapi

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
)

// requestExample is one request body example. Clients such as Postman build a request from the
// first example instead of filling every optional field from the schema.
type requestExample struct {
	name, summary string
	value         map[string]any
}

const modelNote = "Model IDs and efforts come from GET /v1/agent/models; this example uses Claude Code's."

var claudeModel = map[string]any{"id": "sonnet", "effort": "low"}

// requestExamples holds the examples of every operation with a JSON request body. A test checks
// that each one is valid against its schema.
func requestExamples() map[string][]requestExample {
	var logins []requestExample
	catalog := builtin.Catalog()
	for i := range catalog {
		info := &catalog[i]
		logins = append(logins, requestExample{
			name: info.Name, summary: info.Title,
			value: map[string]any{"method": info.LoginMethods[0]},
		})
	}
	one := func(summary string, value map[string]any) []requestExample {
		return []requestExample{{name: "default", summary: summary, value: value}}
	}
	transfer := map[string]any{"source": "src/main.go", "destination": "src/main_copy.go", "conflict": "error"}

	return map[string][]requestExample{
		"startLogin":      logins,
		"submitLoginCode": one("Code from the sign-in page", map[string]any{"code": "<code shown by the sign-in page>"}),
		"ask": one(modelNote, map[string]any{
			"prompt": "Explain HTTP/2 in one sentence.", "model": claudeModel,
		}),
		"askStream": one(modelNote, map[string]any{
			"prompt": "Explain HTTP/2 in one sentence.", "model": claudeModel,
		}),
		"startRun": one(modelNote, map[string]any{
			"workspace_id": "<workspace_id>", "prompt": "Add a README for this project.",
			"model": claudeModel, "tools": map[string]any{"permissions": "ask"},
		}),
		"sendMessage":         one("Next user message", map[string]any{"prompt": "Now add a usage section."}),
		"continueSession":     one("Next user message", map[string]any{"prompt": "Continue where you stopped."}),
		"sendNativeControl":   one("Claude Code control request", map[string]any{"request": map[string]any{"subtype": "get_context_usage"}}),
		"answerNativeControl": one("Answer for the agent", map[string]any{"response": map[string]any{}}),
		"allowPermission":     one("Allow this request only", map[string]any{"scope": "once"}),
		"denyPermission":      one("Reason for the agent", map[string]any{"message": "Do not change files outside src/."}),
		"attachWorkspace":     one("Directory inside NATIVE_AGENT_ALLOWED_ROOTS", map[string]any{"path": "/projects/my-app"}),
		"copyFile":            one("Fail if the destination exists", transfer),
		"moveFile":            one("Fail if the destination exists", transfer),
		"deleteFile":          one("File or directory in the workspace", map[string]any{"path": "notes.txt"}),
		"makeDirectory":       one("Fail if the path exists", map[string]any{"path": "docs", "conflict": "error"}),
	}
}

// documentExamples adds the request examples to the API document.
func documentExamples(document *huma.OpenAPI) {
	examples := requestExamples()
	for _, item := range document.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil || op.RequestBody == nil || op.RequestBody.Content["application/json"] == nil {
				continue
			}
			list, ok := examples[op.OperationID]
			if !ok {
				panic(fmt.Sprintf("httpapi: operation %s has no request example", op.OperationID))
			}
			media := op.RequestBody.Content["application/json"]
			media.Examples = map[string]*huma.Example{}
			for _, example := range list {
				media.Examples[example.name] = &huma.Example{Summary: example.summary, Value: example.value}
			}
		}
	}
}
