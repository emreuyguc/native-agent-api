package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
)

// MinTokenLength is the minimum accepted plain API token length.
const MinTokenLength = 32

// Network configures the listener and the client IP allowlist.
type Network struct {
	// Listen is the explicitly configured ip:port to bind.
	Listen string
	// AllowedIPs restricts clients by source address (NATIVE_AGENT_ALLOWED_IPS, comma
	// separated IPs or CIDRs). Empty allows every source. Forwarding headers such as
	// X-Forwarded-For are never trusted.
	AllowedIPs []netip.Prefix
}

// Allows reports whether a client address passes the allowlist.
func (n Network) Allows(addr netip.Addr) bool {
	if len(n.AllowedIPs) == 0 {
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range n.AllowedIPs {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// Auth configures bearer token authentication.
//
// Only the SHA-256 digest of the token is kept in memory. Prefer NATIVE_AGENT_TOKEN_SHA256
// (or _FILE): agent processes run as the same OS user as the gateway and can read its
// environment, so a plain NATIVE_AGENT_TOKEN would be visible to them while a digest is not
// usable as a credential.
type Auth struct {
	Enabled   bool
	TokenHash [sha256.Size]byte
	// PlainToken reports that the token was configured in clear text.
	PlainToken bool
	// AllowUnauthenticated explicitly permits running without a token on any listener.
	AllowUnauthenticated bool
}

// HashToken returns the SHA-256 digest used for NATIVE_AGENT_TOKEN_SHA256.
func HashToken(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func newAuth(raw spec) (Auth, error) {
	auth := Auth{AllowUnauthenticated: raw.AllowUnauthenticated}
	plain := strings.TrimSpace(raw.Token)
	digests := []string{strings.TrimSpace(raw.TokenSHA256), strings.TrimSpace(raw.TokenSHA256File)}
	configured := 0
	for _, value := range append([]string{plain}, digests...) {
		if value != "" {
			configured++
		}
	}
	if configured > 1 {
		return Auth{}, fmt.Errorf("set only one of %[1]sTOKEN, %[1]sTOKEN_SHA256 and %[1]sTOKEN_SHA256_FILE", EnvPrefix)
	}

	if plain != "" {
		if len(plain) < MinTokenLength {
			return Auth{}, fmt.Errorf("%sTOKEN must contain at least %d characters", EnvPrefix, MinTokenLength)
		}
		auth.Enabled, auth.PlainToken, auth.TokenHash = true, true, HashToken(plain)

		return auth, nil
	}
	for _, digest := range digests {
		if digest == "" {
			continue
		}
		decoded, err := hex.DecodeString(strings.ToLower(digest))
		if err != nil || len(decoded) != sha256.Size {
			return Auth{}, fmt.Errorf("%sTOKEN_SHA256 must be 64 hex characters", EnvPrefix)
		}
		auth.Enabled = true
		copy(auth.TokenHash[:], decoded)
	}

	return auth, nil
}

// ipPrefix parses an IP or CIDR from NATIVE_AGENT_ALLOWED_IPS.
type ipPrefix netip.Prefix

// UnmarshalText implements encoding.TextUnmarshaler (used by caarlos0/env).
func (p *ipPrefix) UnmarshalText(text []byte) error {
	value := strings.TrimSpace(string(text))
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("%q is not an IP or CIDR", value)
		}
		*p = ipPrefix(prefix.Masked())

		return nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return fmt.Errorf("%q is not an IP or CIDR", value)
	}
	addr = addr.Unmap()
	*p = ipPrefix(netip.PrefixFrom(addr, addr.BitLen()))

	return nil
}

func prefixes(values []ipPrefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.Prefix(value))
	}

	return out
}
