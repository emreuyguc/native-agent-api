package cli_test

import (
	"os"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

func TestMain(m *testing.M) {
	buildinfo.Version = "test"
	buildinfo.Commit = "0000000000000000000000000000000000000000"
	buildinfo.Date = "1970-01-01T00:00:00Z"
	os.Exit(m.Run())
}
