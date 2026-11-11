package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// FuzzLiteralHost (F-1): a Host header served without a token names an IP address or localhost
// as a browser reads it, so a web page cannot rebind a DNS name to the gateway.
func FuzzLiteralHost(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1", "127.0.0.1:8080", "[::1]:8080", "::1", "localhost", "LOCALHOST:80", "evil.com",
		"evil.com:80", "127.0.0.1.evil.com", "localhost.evil.com", "[::1]evil.com", "evil.com:80]",
		"[evil.com]:80", "127.1", "0x7f.0.0.1", "[fe80::1%25eth0]:80", "localhost:", ":80", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, host string) {
		if !literalHost(host) {
			return
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return // a bare address, such as ::1
		}
		name := (&url.URL{Host: host}).Hostname()
		if _, err := netip.ParseAddr(name); err != nil && !strings.EqualFold(name, "localhost") {
			t.Fatalf("literalHost(%q) accepted, but a browser resolves the name %q", host, name)
		}
	})
}

// FuzzCrossOrigin (F-2): a state-changing request with an Origin is accepted only when the
// Origin is http or https on exactly this host.
func FuzzCrossOrigin(f *testing.F) {
	for _, seed := range [][2]string{
		{"http://127.0.0.1:8080", "127.0.0.1:8080"},
		{"https://127.0.0.1:8080", "127.0.0.1:8080"},
		{"http://evil.com", "127.0.0.1:8080"},
		{"null", "127.0.0.1:8080"},
		{"file://", "127.0.0.1:8080"},
		{"http://127.0.0.1:8080/", "127.0.0.1:8080"},
		{"HTTP://127.0.0.1:8080", "127.0.0.1:8080"},
		{"http://127.0.0.1:8080.evil.com", "127.0.0.1:8080"},
		{"http://user@127.0.0.1:8080", "127.0.0.1:8080"},
		{"", "127.0.0.1:8080"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, origin, host string) {
		if origin == "" {
			return // Sec-Fetch-Site decides; TestBrowserGuard covers it.
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/runs/start", http.NoBody)
		r.Host = host
		r.Header.Set("Origin", origin)
		if crossOrigin(r) {
			return
		}
		if !strings.EqualFold(origin, "http://"+host) && !strings.EqualFold(origin, "https://"+host) {
			t.Fatalf("Origin %q accepted for host %q", origin, host)
		}
	})
}
