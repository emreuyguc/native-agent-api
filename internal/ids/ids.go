// Package ids generates and validates the opaque identifiers used by the gateway.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/google/uuid"
)

var workspacePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// NewWorkspaceID returns 128 random bits as 32 lowercase hex characters.
func NewWorkspaceID() string {
	var buffer [16]byte
	_, _ = rand.Read(buffer[:]) // crypto/rand.Read never returns an error since Go 1.24.

	return hex.EncodeToString(buffer[:])
}

// NewSessionID returns a random (version 4) UUID, as required by agent CLIs.
func NewSessionID() string { return uuid.NewString() }

// NewRunID returns a time-ordered (version 7) UUID.
func NewRunID() string { return uuid.Must(uuid.NewV7()).String() }

// ValidWorkspaceID reports whether id has the workspace identifier format.
func ValidWorkspaceID(id string) bool { return workspacePattern.MatchString(id) }

// ValidSessionID reports whether id is a canonical lowercase UUID.
func ValidSessionID(id string) bool { return validUUID(id) }

// ValidRunID reports whether id is a canonical lowercase UUID.
func ValidRunID(id string) bool { return validUUID(id) }

func validUUID(id string) bool {
	parsed, err := uuid.Parse(id)

	return err == nil && parsed.String() == id
}
