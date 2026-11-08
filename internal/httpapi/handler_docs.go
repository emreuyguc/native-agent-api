package httpapi

import (
	"net/http"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

// Scalar API reference (https://github.com/scalar/scalar), pinned by version and integrity
// hash. The page loads nothing else from third parties: CSP limits connections to this origin.
const (
	scalarURL       = "https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.76.4/dist/browser/standalone.js"
	scalarIntegrity = "sha384-hwB9WroDTpZaI38gr+u9CXA6ksz5gUYgD2GxXazKl/agT1L2GWUiRFWGJYNiFu4Q"
)

const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + buildinfo.Name + ` API</title>
</head>
<body>
<div id="app"></div>
<script src="` + scalarURL + `" integrity="` + scalarIntegrity + `" crossorigin="anonymous"></script>
<script src="/docs/init.js"></script>
</body>
</html>
`

// The configuration lives in a same-origin file so the CSP needs no inline scripts. Cloud
// features (developer toolbar, Ask AI, telemetry) are off; the CSP would block them anyway.
const docsInit = `Scalar.createApiReference('#app', {
  url: '/openapi.yaml',
  withDefaultFonts: false,
  hideClientButton: true,
  showDeveloperTools: 'never',
  agent: { disabled: true },
  telemetry: false,
  authentication: { preferredSecurityScheme: 'bearer' },
});
`

const docsCSP = "default-src 'none'; script-src 'self' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

func serveSpec(contentType string, spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(spec)
	}
}

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", docsCSP)
	_, _ = w.Write([]byte(docsPage))
}

func (s *Server) docsScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(docsInit))
}
