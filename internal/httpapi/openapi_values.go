package httpapi

import (
	"fmt"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/run"
)

// documentValues writes the fixed values of string fields into the API document. The values come
// from the registries in code (error codes, run error codes, capabilities, adapters), so the
// document cannot drift from what the gateway sends and accepts. Request fields get an enum.
// Response fields that may grow get the list in their description: clients ignore unknown values.
func (s *Server) documentValues(registry huma.Registry) {
	schemas := registry.Map()
	property := func(component, name string) *huma.Schema {
		parent := schemas[component]
		if parent == nil || parent.Properties[name] == nil {
			panic(fmt.Sprintf("httpapi: schema %s has no property %s", component, name))
		}

		return parent.Properties[name]
	}

	meanings := func(values []string, meaning func(string) string) string {
		var b strings.Builder
		b.WriteString("\n")
		for _, value := range values {
			fmt.Fprintf(&b, "\n- `%s`: %s", value, meaning(value))
		}

		return b.String()
	}

	code := property("ApiError", "code")
	code.Description = "Stable machine-readable error code. Clients must accept codes they do not know." +
		meanings(errorCodes(), func(name string) string { return errorCodeMeanings[name] })

	var runCodes []string
	for _, c := range run.ErrorCodes() {
		runCodes = append(runCodes, string(c))
	}
	property("RunErrorInfo", "code").Description = "Stable code. Clients must accept codes they do not know." +
		meanings(runCodes, func(name string) string { return run.ErrorCode(name).Meaning() })

	var capabilities []string
	for _, c := range agent.KnownCapabilities() {
		capabilities = append(capabilities, string(c))
	}
	property("AgentDescription", "capabilities").Items.Description = "Clients must ignore values they do not know." +
		meanings(capabilities, func(name string) string { return agent.Capability(name).Meaning() })

	adapters := s.adapters()
	var names, methods []string
	var perAgent []string
	for i := range adapters {
		info := &adapters[i]
		names = append(names, info.Name)
		perAgent = append(perAgent, info.Title+": `"+strings.Join(info.LoginMethods, "`, `")+"`")
		for _, method := range info.LoginMethods {
			if !slices.Contains(methods, method) {
				methods = append(methods, method)
			}
		}
	}
	setEnum(property("AgentIdentity", "name"), names)

	methodNote := " The served agent accepts only its own methods (GET /v1/agent `login.methods`). " +
		strings.Join(perAgent, ". ") + "."
	for _, field := range []*huma.Schema{
		property("StartLoginInputBody", "method"),
		property("LoginView", "method"),
		property("AgentLogin", "methods").Items,
	} {
		setEnum(field, methods)
		field.Description = strings.TrimSpace(field.Description + methodNote)
	}
}

// adapters are the builtin adapters, plus the served agent when it has no builtin entry (the
// test fake), so its own values are valid too.
func (s *Server) adapters() []agent.Info {
	adapters := builtin.Catalog()
	if s.agent != nil && !slices.ContainsFunc(adapters, func(info agent.Info) bool { return info.Name == s.agent.Name() }) {
		adapters = append(adapters, s.agent.Info())
	}

	return adapters
}

func setEnum(schema *huma.Schema, values []string) {
	schema.Enum = nil
	for _, value := range values {
		schema.Enum = append(schema.Enum, value)
	}
	schema.PrecomputeMessages()
}
