// Package buildinfo exposes product identity and build metadata.
//
// Release builds set the variables with:
//
//	-ldflags "-X github.com/emreuyguc/native-agent-api/internal/buildinfo.Version=v0.1.0 ..."
//
// Commands that expose or serve build identity reject binaries where these values were not set.
package buildinfo

import (
	"errors"
	"fmt"
	"runtime"
)

// Name is the canonical executable and product name.
const Name = "native-agent-api"

// Set via -ldflags -X at build time.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Validate rejects binaries built without explicit identity metadata.
func Validate() error {
	var errs []error
	for _, item := range []struct{ name, value string }{
		{"version", Version}, {"commit", Commit}, {"date", Date},
	} {
		if item.value == "" {
			errs = append(errs, fmt.Errorf("build %s is missing", item.name))
		}
	}

	return errors.Join(errs...)
}

// Info is the build metadata reported by the CLI and the HTTP API.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns build metadata injected by the build command.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders one human-readable line.
func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf("%s %s (%s, %s, %s)", Name, i.Version, commit, i.GoVersion, i.Platform)
}
