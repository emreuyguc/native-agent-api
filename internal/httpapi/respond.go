package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/emreuyguc/native-agent-api/internal/logx"
)

// problemType is the media type of every error response (RFC 9457).
const problemType = "application/problem+json"

// apiError is the single error format of the API: RFC 9457 Problem Details with a stable
// code, the request ID and, for validation failures, one entry per invalid field. The type
// member is omitted, which RFC 9457 defines as "about:blank": the title is the HTTP status
// text and code identifies the problem.
type apiError struct {
	Title     string       `json:"title"                doc:"HTTP status text." example:"Unprocessable Entity"`
	Status    int          `json:"status"               doc:"HTTP status code." example:"422"`
	Detail    string       `json:"detail"               doc:"Human-readable explanation of this occurrence."`
	Code      string       `json:"code"                 doc:"Stable machine-readable error code." example:"invalid_request"`
	RequestID string       `json:"request_id,omitempty" doc:"The X-Request-ID of the request; also in the gateway logs."`
	Errors    []fieldError `json:"errors,omitempty"     doc:"Invalid fields (validation failures)."`
}

// fieldError names one invalid field. The offending value is never echoed: it may be a
// whole (large, sensitive) prompt.
type fieldError struct {
	Location string `json:"location" doc:"Where the field is, e.g. body.model.effort." example:"body.model.effort"`
	Message  string `json:"message"`
}

func (e *apiError) Error() string             { return e.Detail }
func (e *apiError) GetStatus() int            { return e.Status }
func (e *apiError) ContentType(string) string { return problemType }

// fail returns an API error with an explicit code.
func fail(status int, code errorCode, message string) error {
	return newProblem(status, code, message)
}

func newProblem(status int, code errorCode, message string) *apiError {
	return &apiError{Title: http.StatusText(status), Status: status, Detail: message, Code: code.name}
}

// Errors produced by Huma itself (validation, body limits) use the same format; the code is
// derived from the status.
func init() { //nolint:gochecknoinits // Huma exposes its error constructor as a package variable.
	huma.NewError = func(status int, message string, errs ...error) huma.StatusError {
		problem := newProblem(status, statusCode(status), message)
		for _, err := range errs {
			switch detail := err.(type) { //nolint:errorlint // Huma passes *ErrorDetail values directly.
			case nil:
			case *huma.ErrorDetail:
				problem.Errors = append(problem.Errors, fieldError{Location: detail.Location, Message: detail.Message})
			default:
				problem.Errors = append(problem.Errors, fieldError{Message: err.Error()})
			}
		}

		return problem
	}
}

// withRequestID is a Huma transformer: it stamps the request ID on every error body.
func withRequestID(ctx huma.Context, _ string, value any) (any, error) {
	if problem, ok := value.(*apiError); ok {
		problem.RequestID = logx.RequestID(ctx.Context())
	}

	return value, nil
}

func statusCode(status int) errorCode {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codeInvalidRequest
	case http.StatusRequestEntityTooLarge:
		return codeRequestTooLarge
	case http.StatusRequestTimeout:
		return codeRequestTimeout
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case http.StatusUnsupportedMediaType:
		return codeUnsupportedMediaType
	case http.StatusNotAcceptable:
		return codeNotAcceptable
	default:
		return codeInternalError
	}
}

// writeJSON and writeError are used where no Huma operation is involved: middleware and
// streaming responses.
func writeJSON(w http.ResponseWriter, status int, value any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value) //nolint:errchkjson // Client disconnects are not actionable.
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code errorCode, message string) {
	problem := newProblem(status, code, message)
	problem.RequestID = logx.RequestID(r.Context())
	w.Header().Set("Content-Type", problemType)
	writeJSON(w, status, problem)
}

// jsonObject is raw JSON that must be an object. It is passed through unchanged.
type jsonObject json.RawMessage

func (o jsonObject) MarshalJSON() ([]byte, error) {
	if len(o) == 0 {
		return []byte("null"), nil
	}
	return o, nil
}

func (o *jsonObject) UnmarshalJSON(data []byte) error {
	*o = append((*o)[:0], data...)
	return nil
}

// Schema documents jsonObject as a free-form object (Huma validates requests against it).
func (jsonObject) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeObject, AdditionalProperties: true}
}

func isJSONObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// body wraps a JSON response body.
type body[T any] struct {
	Body T
}

func reply[T any](value T) *body[T] { return &body[T]{Body: value} }
