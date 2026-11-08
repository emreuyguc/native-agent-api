package buildinfo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

func TestNameMatchesModule(t *testing.T) {
	t.Parallel()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleFile := filepath.Join(filepath.Dir(filepath.Dir(dir)), "go.mod")
	content, err := os.ReadFile(moduleFile)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(content), "\n")
	module, ok := strings.CutPrefix(line, "module ")
	if !ok {
		t.Fatalf("first go.mod line is %q", line)
	}
	if name := filepath.Base(strings.TrimSpace(module)); name != buildinfo.Name {
		t.Fatalf("buildinfo.Name = %q, module name = %q", buildinfo.Name, name)
	}
}
