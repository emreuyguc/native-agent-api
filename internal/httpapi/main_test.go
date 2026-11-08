package httpapi_test

import (
	"os"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

func TestMain(m *testing.M) {
	buildinfo.Version = "test"
	os.Exit(m.Run())
}
