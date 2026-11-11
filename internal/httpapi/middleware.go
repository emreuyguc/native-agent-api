package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/emreuyguc/native-agent-api/internal/logx"
	"github.com/emreuyguc/native-agent-api/internal/redact"
)

// Probe paths are reachable without token and allowlist so container orchestration can use them.
const (
	healthPath = "/health"
	readyPath  = "/ready"
)

func isProbe(path string) bool { return path == healthPath || path == readyPath }

// requestIDHeader carries the request's correlation ID in both directions.
const requestIDHeader = "X-Request-ID"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:/+=-]{1,128}$`)

// requestID accepts the client's X-Request-ID or creates a time-ordered UUIDv7. The ID is
// returned in the response header and error bodies, and logged with every record of the
// request. An invalid client ID is rejected rather than replaced, so the client never
// searches the logs for an ID the gateway did not use.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		valid := id == "" || requestIDPattern.MatchString(id)
		if !valid || id == "" {
			id = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set(requestIDHeader, id)
		r = r.WithContext(logx.WithRequestID(r.Context(), id))
		if !valid {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequestID,
				requestIDHeader+" must be 1 to 128 characters from A-Z a-z 0-9 . _ : / + = -")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// allowlist rejects clients whose TCP source address is not allowed. Forwarding headers are
// ignored on purpose: they are controlled by the client.
func (s *Server) allowlist(next http.Handler) http.Handler {
	if len(s.cfg.Network.AllowedIPs) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isProbe(r.URL.Path) {
			addr, err := netip.ParseAddrPort(r.RemoteAddr)
			if err != nil || !s.cfg.Network.Allows(addr.Addr()) {
				s.log.WarnContext(r.Context(), "client address rejected by allowlist", "remote", r.RemoteAddr, "path", r.URL.Path)
				writeError(w, r, http.StatusForbidden, codeIPNotAllowed, "Client address is not allowed.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// browserGuard stops browser-borne requests (docs/security.md F-1, F-2). Without a token a web
// page could otherwise reach the gateway: by DNS rebinding a name of its own to the listener, or
// with a cross-site form POST. Non-browser clients send no Origin and are not affected.
func (s *Server) browserGuard(next http.Handler) http.Handler {
	checkHost := !s.cfg.Auth.Enabled

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkHost && !literalHost(r.Host) {
			s.log.WarnContext(r.Context(), "request host rejected", "host", r.Host, "path", r.URL.Path)
			writeError(w, r, http.StatusForbidden, codeHostNotAllowed,
				"Without a token the gateway must be addressed by IP address or localhost.")
			return
		}
		if !safeMethod(r.Method) && crossOrigin(r) {
			s.log.WarnContext(r.Context(), "cross-origin request rejected", "origin", r.Header.Get("Origin"), "path", r.URL.Path)
			writeError(w, r, http.StatusForbidden, codeCrossOriginRequest, "Cross-origin requests are not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// literalHost accepts an IP address or localhost, with an optional numeric port: names a web page
// cannot rebind. Anything that does not parse as host and port, or as a bare host, is rejected.
func literalHost(host string) bool {
	name := host
	if split, port, err := net.SplitHostPort(host); err == nil {
		if strings.Trim(port, "0123456789") != "" {
			return false
		}
		name = split
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		name = host[1 : len(host)-1]
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}

	return strings.EqualFold(name, "localhost")
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// crossOrigin reports a browser request from another origin: an Origin that does not name this
// host, an opaque "null" origin, or Sec-Fetch-Site cross-site/same-site without an Origin.
func crossOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "cross-site" || site == "same-site"
	}
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return true
	}

	return !strings.EqualFold(rest, r.Host)
}

// authenticate compares the SHA-256 digest of the bearer token in constant time. Every
// path except the public ones (health, docs) needs it, including unknown paths.
func (s *Server) authenticate(next http.Handler, public map[string]bool) http.Handler {
	if !s.cfg.Auth.Enabled {
		return next
	}
	expected := s.cfg.Auth.TokenHash

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		digest := sha256.Sum256([]byte(provided))
		if !ok || subtle.ConstantTimeCompare(digest[:], expected[:]) != 1 {
			s.log.WarnContext(r.Context(), "authentication failed", "remote", r.RemoteAddr, "path", r.URL.Path, "bearer", ok)
			writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "A valid bearer token is required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests writes one access log record per request. With Log.Bodies it also records
// truncated, redacted request and response bodies (never for file transfers).
func (s *Server) logRequests(next http.Handler) http.Handler {
	logCfg := s.cfg.Log
	if !logCfg.Requests {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		withBodies := logCfg.Bodies && logCfg.BodyLimit > 0 && !strings.Contains(r.URL.Path, "/files") &&
			!strings.HasPrefix(r.URL.Path, "/v1/agent/login")
		var requestBody *limitedBuffer
		if withBodies {
			requestBody = &limitedBuffer{limit: logCfg.BodyLimit}
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(r.Body, requestBody), r.Body}
			recorder.body = &limitedBuffer{limit: logCfg.BodyLimit}
		}

		next.ServeHTTP(recorder, r)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.Int("status", recorder.status),
			slog.Int64("bytes", recorder.written),
			slog.Duration("duration", time.Since(start)),
		}
		if withBodies {
			attrs = append(attrs,
				slog.String("request_body", requestBody.String()),
				slog.String("response_body", recorder.body.String()),
			)
		}
		// Failures are logged where they happen; the access record itself stays at INFO
		// (DEBUG for health checks so they do not flood the log).
		level := slog.LevelInfo
		if isProbe(r.URL.Path) {
			level = slog.LevelDebug
		}
		s.log.LogAttrs(r.Context(), level, "request", attrs...)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler { //nolint:errorlint // Sentinel compared by identity per net/http docs.
					panic(recovered)
				}
				s.log.ErrorContext(ctx, "handler panic", slog.Any("panic", recovered), slog.String("path", r.URL.Path))
				writeError(w, r, http.StatusInternalServerError, codeInternalError, "Internal server error.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseRecorder captures status, size and (optionally) the start of the body. Unwrap
// keeps http.ResponseController features (Flush, deadlines) working through the wrapper.
type responseRecorder struct {
	http.ResponseWriter

	status  int
	written int64
	body    *limitedBuffer
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.body != nil {
		_, _ = r.body.Write(data)
	}
	n, err := r.ResponseWriter.Write(data)
	r.written += int64(n)

	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// limitedBuffer keeps the first limit bytes written and counts the rest.
type limitedBuffer struct {
	buffer  bytes.Buffer
	limit   int
	dropped int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	take := min(max(b.limit-b.buffer.Len(), 0), len(data))
	b.buffer.Write(data[:take])
	b.dropped += len(data) - take

	return len(data), nil
}

func (b *limitedBuffer) String() string {
	text := redact.Message(b.buffer.String(), 0)
	if b.dropped > 0 {
		text += "…[truncated]"
	}

	return text
}
