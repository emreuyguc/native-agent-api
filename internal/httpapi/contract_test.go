package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent/agenttest"
	"github.com/emreuyguc/native-agent-api/internal/httpapi"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

var contract = sync.OnceValues(func() (*openapi3.T, error) {
	// The document a server serving the test agent publishes: the builtin agents plus its own
	// provider sections.
	spec, err := httpapi.OpenAPIFor(&agenttest.Agent{})
	if err != nil {
		return nil, err
	}
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, err
	}

	return doc, doc.Validate(context.Background())
})

var contractRouter = sync.OnceValues(func() (routers.Router, error) {
	doc, err := contract()
	if err != nil {
		return nil, err
	}

	return gorillamux.NewRouter(doc)
})

// checkContract validates JSON responses (and the requests of successful calls) against the
// OpenAPI document. Streams and binary bodies are skipped.
func checkContract(t *testing.T, req *http.Request, payload []byte, resp *http.Response, data []byte) {
	t.Helper()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "application/json" && mediaType != "application/problem+json" {
		return
	}
	router, err := contractRouter()
	if err != nil {
		t.Fatalf("load OpenAPI document: %v", err)
	}
	probe := req.Clone(context.Background())
	probe.URL.Host, probe.URL.Scheme, probe.Host = "", "", ""
	probe.Body = io.NopCloser(bytes.NewReader(payload))
	// {path...} spans segments; the OpenAPI router matches one segment, so route on the first.
	for _, action := range []string{"/files/download/", "/files/upload/"} {
		if prefix, rest, ok := strings.Cut(probe.URL.Path, action); ok {
			probe.URL.Path = prefix + action + strings.ReplaceAll(rest, "/", "_")
			probe.URL.RawPath = ""
		}
	}
	route, params, err := router.FindRoute(probe)
	if err != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) {
		return // An unknown route: the gateway answered with its generic problem.
	}
	if err != nil {
		t.Fatalf("%s %s is not in api/openapi.yaml: %v", req.Method, req.URL.Path, err)
	}
	input := &openapi3filter.RequestValidationInput{
		Request: probe, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if resp.StatusCode < 400 && len(payload) > 0 {
		if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
			t.Errorf("request %s %s violates the contract: %v", req.Method, req.URL.Path, err)
		}
	}
	err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Body:                   io.NopCloser(bytes.NewReader(data)),
	})
	if err != nil {
		t.Errorf("response %d for %s %s violates the contract: %v\nbody: %s", resp.StatusCode, req.Method, req.URL.Path, err, data)
	}
}

func TestDocs(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Docs = true
	server := newServer(t, cfg, &agenttest.Agent{})

	resp, body := do(t, http.MethodGet, server.URL+"/docs", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `integrity="sha384-`) ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatalf("docs: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, body = do(t, http.MethodGet, server.URL+"/openapi.yaml", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "openapi: 3.1") {
		t.Fatalf("openapi.yaml: %d", resp.StatusCode)
	}

	cfg.Docs = false
	disabled := newServer(t, cfg, &agenttest.Agent{})
	if resp, _ = do(t, http.MethodGet, disabled.URL+"/docs", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("docs disabled: %d", resp.StatusCode)
	}
}
