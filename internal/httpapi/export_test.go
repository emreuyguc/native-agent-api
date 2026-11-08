package httpapi

import (
	"net/http"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/config"
)

// SetStreamWriteTimeout shortens the stream write timeout for one test.
func SetStreamWriteTimeout(d time.Duration) (restore func()) {
	previous := streamWriteTimeout
	streamWriteTimeout = d

	return func() { streamWriteTimeout = previous }
}

// OpenAPIFor is the document a server serving a builds: every builtin agent plus a.
func OpenAPIFor(a agent.Agent) ([]byte, error) {
	s := &Server{cfg: config.Config{Limits: config.Limits{MaxBodyBytes: 1 << 20}}, agent: a}
	return s.newAPI(http.NewServeMux()).OpenAPI().YAML()
}
