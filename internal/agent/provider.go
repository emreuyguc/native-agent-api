package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Provider is the provider section of an API object: {"name": "<agent>", the fields only that
// agent has, "native": its unmodified native data}. Everything outside the section is gateway
// data or data every agent supplies with the same meaning.
type Provider struct {
	// Name is the served agent ("claude"); it selects the section's schema.
	Name string
	// Fields is an adapter-defined struct with the agent's own typed fields, or nil.
	Fields any
	// Native is the unmodified native document, or nil.
	Native json.RawMessage
}

// NewProvider returns a provider section.
func NewProvider(name string, fields any, native json.RawMessage) Provider {
	return Provider{Name: name, Fields: fields, Native: native}
}

// MarshalJSON writes name first, then the adapter fields, then native.
func (p Provider) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	name, err := json.Marshal(p.Name)
	if err != nil {
		return nil, err
	}
	out.WriteString(`{"name":`)
	out.Write(name)
	if p.Fields != nil {
		fields, err := json.Marshal(p.Fields)
		if err != nil {
			return nil, err
		}
		fields = bytes.TrimSpace(fields)
		if len(fields) < 2 || fields[0] != '{' {
			return nil, errors.New("provider fields must encode as a JSON object")
		}
		if inner := bytes.TrimSpace(fields[1 : len(fields)-1]); len(inner) > 0 {
			out.WriteByte(',')
			out.Write(inner)
		}
	}
	if len(p.Native) > 0 {
		out.WriteString(`,"native":`)
		out.Write(p.Native)
	}
	out.WriteByte('}')

	return out.Bytes(), nil
}

// UnmarshalJSON reads a section written by MarshalJSON; Fields becomes the remaining members as
// a json.RawMessage object.
func (p *Provider) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	*p = Provider{}
	if err := json.Unmarshal(members["name"], &p.Name); err != nil {
		return fmt.Errorf("provider.name: %w", err)
	}
	p.Native = members["native"]
	delete(members, "name")
	delete(members, "native")
	if len(members) > 0 {
		fields, err := json.Marshal(members)
		if err != nil {
			return err
		}
		p.Fields = json.RawMessage(fields)
	}

	return nil
}

// Provider sections of response objects. Each is its own type so the OpenAPI document can give
// it a schema with one variant per agent (see Section*).
type (
	// AuthProvider is the provider section of an auth status.
	AuthProvider struct{ Provider }
	// SubscriptionProvider is the provider section of the subscription usage.
	SubscriptionProvider struct{ Provider }
	// ModelProvider is the provider section of a model.
	ModelProvider struct{ Provider }
	// ResultProvider is the provider section of a turn result.
	ResultProvider struct{ Provider }
	// PermissionProvider is the provider section of a permission request.
	PermissionProvider struct{ Provider }
	// ControlProvider is the provider section of a control request from the agent.
	ControlProvider struct{ Provider }
)

// Section names an API object that has a provider section. Request sections carry provider
// options from the client; response sections carry provider data to the client.
type Section string

// Sections.
const (
	SectionRun          Section = "run"          // request: run settings (provider options)
	SectionLogin        Section = "login"        // request: login start
	SectionAllow        Section = "allow"        // request: permission allow
	SectionAuth         Section = "auth"         // response: AuthProvider
	SectionSubscription Section = "subscription" // response: SubscriptionProvider
	SectionModel        Section = "model"        // response: ModelProvider
	SectionResult       Section = "result"       // response: ResultProvider
	SectionPermission   Section = "permission"   // response: PermissionProvider
	SectionControl      Section = "control"      // response: ControlProvider
)

// RequestSections are the sections clients send.
var RequestSections = []Section{SectionRun, SectionLogin, SectionAllow}

// ResponseSections map response sections to their Go type.
var ResponseSections = map[Section]reflect.Type{
	SectionAuth:         reflect.TypeFor[AuthProvider](),
	SectionSubscription: reflect.TypeFor[SubscriptionProvider](),
	SectionModel:        reflect.TypeFor[ModelProvider](),
	SectionResult:       reflect.TypeFor[ResultProvider](),
	SectionPermission:   reflect.TypeFor[PermissionProvider](),
	SectionControl:      reflect.TypeFor[ControlProvider](),
}

// SectionDescriber is implemented by adapters to describe their provider sections: the Go type
// of the agent's own fields per section. A missing section has no fields besides name (and
// native, for responses). The OpenAPI document is generated from it.
type SectionDescriber interface {
	ProviderSections() map[Section]reflect.Type
}

// ProviderMismatchError reports a provider section written for another agent.
type ProviderMismatchError struct {
	Field, Provider, Served string
}

func (e *ProviderMismatchError) Error() string {
	return fmt.Sprintf("%s.name is %q but this gateway serves %q", e.Field, e.Provider, e.Served)
}

// SplitProvider checks a request's provider section (field names it in errors) against the
// served agent and returns the agent's own fields without name. Empty input yields nil.
func SplitProvider(raw json.RawMessage, field, served string) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, Invalid("%s must be a JSON object", field)
	}
	var name string
	if err := json.Unmarshal(members["name"], &name); err != nil || name == "" {
		return nil, Invalid("%s.name is required", field)
	}
	if name != served {
		return nil, &ProviderMismatchError{Field: field, Provider: name, Served: served}
	}
	delete(members, "name")
	if len(members) == 0 {
		return nil, nil
	}
	fields, err := json.Marshal(members)
	if err != nil {
		return nil, Invalid("%s: %v", field, err)
	}

	return fields, nil
}

// DecodeStrict decodes an adapter's provider fields into target, rejecting unknown members.
// Empty input leaves target unchanged.
func DecodeStrict(raw json.RawMessage, field string, target any) error {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return Invalid("%s: %v", field, err)
	}

	return nil
}
