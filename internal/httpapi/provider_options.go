package httpapi

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Request provider sections: {"name": "<agent>", the agent's own fields}. Each type points at
// the component built by providerComponents.
type (
	runProvider   json.RawMessage
	loginProvider json.RawMessage
	allowProvider json.RawMessage
)

// requestSectionComponents names the schema component of each request section.
var requestSectionComponents = map[agent.Section]string{
	agent.SectionRun:   "RunProvider",
	agent.SectionLogin: "LoginProvider",
	agent.SectionAllow: "AllowProvider",
}

func (o runProvider) MarshalJSON() ([]byte, error)   { return jsonObject(o).MarshalJSON() }
func (o loginProvider) MarshalJSON() ([]byte, error) { return jsonObject(o).MarshalJSON() }
func (o allowProvider) MarshalJSON() ([]byte, error) { return jsonObject(o).MarshalJSON() }

func (o *runProvider) UnmarshalJSON(data []byte) error { *o = append((*o)[:0], data...); return nil }

func (o *loginProvider) UnmarshalJSON(data []byte) error { *o = append((*o)[:0], data...); return nil }

func (o *allowProvider) UnmarshalJSON(data []byte) error { *o = append((*o)[:0], data...); return nil }

func (runProvider) Schema(huma.Registry) *huma.Schema   { return sectionRef(agent.SectionRun) }
func (loginProvider) Schema(huma.Registry) *huma.Schema { return sectionRef(agent.SectionLogin) }
func (allowProvider) Schema(huma.Registry) *huma.Schema { return sectionRef(agent.SectionAllow) }

func sectionRef(section agent.Section) *huma.Schema {
	return &huma.Schema{Ref: "#/components/schemas/" + requestSectionComponents[section]}
}

// sectionDescriptions explain each section in the document.
var sectionDescriptions = map[agent.Section]string{
	agent.SectionRun:          "Options only the served agent has (docs/agents/); omitted options keep native behavior.",
	agent.SectionLogin:        "Login options only the served agent has.",
	agent.SectionAllow:        "Allow options only the served agent has.",
	agent.SectionAuth:         "The agent's own login details and its unmodified status document (`native`).",
	agent.SectionSubscription: "The agent's own usage details and its unmodified usage document (`native`).",
	agent.SectionModel:        "The unmodified native model entry (`native`).",
	agent.SectionResult:       "The native outcome code, the agent's own usage data and the unmodified native result (`native`).",
	agent.SectionPermission:   "The native tool or request method and the unmodified native request (`native`).",
	agent.SectionControl:      "The native request type and the unmodified native request (`native`).",
}

// providerComponents registers one schema per provider section: a oneOf with one variant per
// agent, selected by `name`. sections maps each agent to the Go types of its own fields; a
// missing section has no fields besides name (and native for responses).
func providerComponents(registry huma.Registry, sections map[string]map[agent.Section]reflect.Type) {
	inlined := map[string]bool{}
	for _, section := range agent.RequestSections {
		providerComponent(registry, section, requestSectionComponents[section], sections, false, inlined)
	}
	for _, section := range slices.Sorted(maps.Keys(agent.ResponseSections)) {
		t := agent.ResponseSections[section]
		component := schemaName(t, t.Name())
		registry.Schema(t, true, "") // Registers the type so later references point at component.
		providerComponent(registry, section, component, sections, true, inlined)
	}
	// The adapters' field types are inlined into the variants; only the variants are part of the API.
	for name := range inlined {
		delete(registry.Map(), name)
	}
}

func providerComponent(registry huma.Registry, section agent.Section, component string, sections map[string]map[agent.Section]reflect.Type, response bool, inlined map[string]bool) {
	schemas := registry.Map()
	var variants []*huma.Schema
	mapping := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		variant := &huma.Schema{Type: huma.TypeObject, AdditionalProperties: false}
		if t := sections[name][section]; t != nil {
			base := registry.Schema(t, false, "")
			inlined[schemaName(t, t.Name())] = true
			copied := *base
			variant = &copied
		}
		variant.Properties = maps.Clone(variant.Properties)
		if variant.Properties == nil {
			variant.Properties = map[string]*huma.Schema{}
		}
		nameSchema := &huma.Schema{Type: huma.TypeString, Description: "The agent: `" + name + "`."}
		if !response {
			nameSchema.Description += " Another agent's name is rejected with `provider_mismatch`."
		}
		nameSchema.Enum = []any{name}
		variant.Properties["name"] = nameSchema
		if response {
			variant.Properties["native"] = &huma.Schema{Description: "Unmodified native data."}
		}
		variant.Required = append([]string{"name"}, slices.DeleteFunc(slices.Clone(variant.Required), func(field string) bool { return field == "name" })...)
		variant.Description = sectionDescriptions[section]
		variant.PrecomputeMessages()
		variantName := strings.ToUpper(name[:1]) + name[1:] + strings.TrimPrefix(component, "Agent")
		schemas[variantName] = variant
		variants = append(variants, &huma.Schema{Ref: "#/components/schemas/" + variantName})
		mapping[name] = "#/components/schemas/" + variantName
	}
	combined := &huma.Schema{
		Description:   "Provider section: " + sectionDescriptions[section] + " `name` selects the variant.",
		OneOf:         variants,
		Discriminator: &huma.Discriminator{PropertyName: "name", Mapping: mapping},
	}
	combined.PrecomputeMessages()
	schemas[component] = combined
}
