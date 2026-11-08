package claude_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent/claude"
)

type helpSurface struct {
	Version                    string                         `json:"version"`
	Normalization              string                         `json:"normalization"`
	RootOptions                map[string][]string            `json:"root_options"`
	HiddenOptionsUsedByAdapter []string                       `json:"hidden_options_used_by_adapter"`
	RootCommands               map[string][]string            `json:"root_commands"`
	AuthCommands               map[string][]string            `json:"auth_commands"`
	CommandOptions             map[string]map[string][]string `json:"command_options"`
}

func TestPinnedHelpSurfaceIsCompletelyClassified(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/2.1.318-surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var surface helpSurface
	if err := json.Unmarshal(data, &surface); err != nil {
		t.Fatal(err)
	}
	if surface.Version != claude.Descriptor.TestedVersion {
		t.Fatalf("help surface version %s does not match adapter version %s", surface.Version, claude.Descriptor.TestedVersion)
	}
	if surface.Normalization == "" {
		t.Fatal("help surface normalization is not documented")
	}
	for section, groups := range map[string]map[string][]string{
		"root option": surface.RootOptions, "root command": surface.RootCommands, "auth command": surface.AuthCommands,
	} {
		checkClassifications(t, section, groups)
	}
	for command, groups := range surface.CommandOptions {
		checkClassifications(t, command+" option", groups)
	}
	for _, required := range []string{"--max-turns", "--permission-prompt-tool", "--system-prompt-file"} {
		if !slices.Contains(surface.HiddenOptionsUsedByAdapter, required) {
			t.Errorf("hidden adapter option %s is not recorded", required)
		}
	}
}

func checkClassifications(t *testing.T, section string, groups map[string][]string) {
	t.Helper()
	seen := map[string]string{}
	for classification, values := range groups {
		if len(values) == 0 {
			t.Errorf("%s classification %q is empty", section, classification)
		}
		if !slices.IsSorted(values) {
			t.Errorf("%s classification %q must be sorted: %v", section, classification, values)
		}
		for _, value := range values {
			if previous, exists := seen[value]; exists {
				t.Errorf("%s %q is classified as both %s and %s", section, value, previous, classification)
			}
			seen[value] = classification
		}
	}
}
