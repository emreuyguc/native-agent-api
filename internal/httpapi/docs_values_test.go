package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/run"
)

func builtinDocument(t *testing.T) *openapi3.T {
	t.Helper()
	spec, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}

	return doc
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// enumValues collects the enum values of schema and its inline children, keyed by their
// location. A reference is another component, checked on its own.
func enumValues(location string, schema *openapi3.SchemaRef, found map[string][]string, seen map[*openapi3.Schema]bool) {
	if schema == nil || schema.Ref != "" || schema.Value == nil || seen[schema.Value] {
		return
	}
	seen[schema.Value] = true
	for _, value := range schema.Value.Enum {
		if text, ok := value.(string); ok {
			found[location] = append(found[location], text)
		}
	}
	for name, property := range schema.Value.Properties {
		enumValues(location+"."+name, property, found, seen)
	}
	enumValues(location+"[]", schema.Value.Items, found, seen)
	for _, variant := range slices.Concat(schema.Value.OneOf, schema.Value.AnyOf, schema.Value.AllOf) {
		enumValues(location, variant, found, seen)
	}
}

// TestFixedValuesAreDocumented: every fixed value in the API document appears in the Markdown
// docs. A provider variant's values belong in its agent's support document, all others in
// docs/api.md. The variant's own `name` is the agent name, checked by the README test.
func TestFixedValuesAreDocumented(t *testing.T) {
	t.Parallel()
	doc := builtinDocument(t)
	owners := map[string]string{} // schema name prefix of an agent's provider variants -> its doc
	for _, info := range builtin.Catalog() {
		owners[strings.ToUpper(info.Name[:1])+info.Name[1:]] = info.Docs
	}
	texts := map[string]string{"docs/api.md": readDoc(t, "docs/api.md")}
	for _, path := range owners {
		texts[path] = readDoc(t, path)
	}
	for name, schema := range doc.Components.Schemas {
		owner := "docs/api.md"
		for prefix, path := range owners {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, "Provider") {
				owner = path
			}
		}
		found := map[string][]string{}
		enumValues(name, schema, found, map[*openapi3.Schema]bool{})
		for location, values := range found {
			if owner != "docs/api.md" && location == name+".name" {
				continue
			}
			for _, value := range values {
				if !strings.Contains(texts[owner], "`"+value+"`") {
					t.Errorf("%s: value `%s` of %s is not documented", owner, value, location)
				}
			}
		}
	}
}

// TestValueTablesMatchCode: docs/api.md describes every error code, run error code and
// capability with the meaning the API document gives.
func TestValueTablesMatchCode(t *testing.T) {
	t.Parallel()
	text := readDoc(t, "docs/api.md")
	row := func(value, meaning string) {
		want := "| `" + value + "` | " + meaning + " |"
		if !strings.Contains(text, want) {
			t.Errorf("docs/api.md: missing or stale row %q", want)
		}
	}
	for _, code := range errorCodes() {
		row(code, errorCodeMeanings[code])
	}
	for _, code := range run.ErrorCodes() {
		row(string(code), code.Meaning())
	}
	for _, capability := range agent.KnownCapabilities() {
		if capability.Meaning() == "" {
			t.Errorf("capability %s has no meaning", capability)
		}
		row(string(capability), capability.Meaning())
	}
}

// TestRequestExamplesAreValid: every JSON request body has examples, each valid against its
// schema, so clients that build requests from them send a valid body.
func TestRequestExamplesAreValid(t *testing.T) {
	t.Parallel()
	doc := builtinDocument(t)
	used := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody == nil || op.RequestBody.Value.Content.Get("application/json") == nil {
				continue
			}
			media := op.RequestBody.Value.Content.Get("application/json")
			if len(media.Examples) == 0 {
				t.Errorf("%s %s: no request example", method, path)
			}
			used[op.OperationID] = true
			for name, example := range media.Examples {
				if err := media.Schema.Value.VisitJSON(example.Value.Value); err != nil {
					t.Errorf("%s %s: example %s is invalid: %v", method, path, name, err)
				}
			}
		}
	}
	for id := range requestExamples() {
		if !used[id] {
			t.Errorf("request example for unknown operation %s", id)
		}
	}
}

// TestWebSocketTypesAreDocumented: every WebSocket message type of the API document is in
// docs/api.md.
func TestWebSocketTypesAreDocumented(t *testing.T) {
	t.Parallel()
	text := readDoc(t, "docs/api.md")
	types := regexp.MustCompile(`\{\\?"type\\?":\\?"([a-z_]+)\\?"`).FindAllStringSubmatch(wsDescription, -1)
	if len(types) == 0 {
		t.Fatal("no message types in wsDescription")
	}
	for _, match := range types {
		if !strings.Contains(text, "`"+match[1]+"`") {
			t.Errorf("docs/api.md: WebSocket type `%s` is not documented", match[1])
		}
	}
}
