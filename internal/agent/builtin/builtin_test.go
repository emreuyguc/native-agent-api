package builtin_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/builtin"
	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/logx"
)

func TestBuildRejectsUnknownAgent(t *testing.T) {
	t.Parallel()

	_, err := builtin.Build(config.Config{Agent: "gemini"}, logx.Discard())
	if err == nil || !strings.Contains(err.Error(), `unknown agent "gemini"`) {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestBuildCodex(t *testing.T) {
	t.Parallel()
	built, err := builtin.Build(config.Config{
		Agent: "codex", SandboxMode: "external", HomeDir: "/home/agent", User: "agent", SearchPath: "/usr/bin",
		TempDir: "/tmp", WorkDir: "/work", ProfileDir: "native",
		Limits: config.Limits{MaxOutputBytes: 1 << 20, CommandTimeout: time.Second},
	}, logx.Discard())
	if err != nil || built.Name() != "codex" {
		t.Fatalf("Build() = %T, %v", built, err)
	}
}

func TestCatalogIsDocumented(t *testing.T) {
	t.Parallel()

	catalog := builtin.Catalog()
	known := agent.KnownCapabilities()
	if !slices.IsSorted(known) {
		t.Fatalf("known capabilities must be sorted: %v", known)
	}
	readme, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for i := range catalog {
		info := &catalog[i]
		if info.Name == "" || info.Title == "" || info.Provider == "" || info.TestedVersion == "" {
			t.Errorf("incomplete descriptor %+v", info)
		}
		if _, err := os.Stat("../../../" + info.Docs); err != nil {
			t.Errorf("%s: support matrix %s is missing: %v", info.Name, info.Docs, err)
		}
		if len(info.Capabilities) == 0 || !slices.IsSorted(info.Capabilities) {
			t.Errorf("%s: capabilities must be non-empty and sorted: %v", info.Name, info.Capabilities)
		}
		for i := 1; i < len(info.Capabilities); i++ {
			if info.Capabilities[i] == info.Capabilities[i-1] {
				t.Errorf("%s: duplicate capability %q", info.Name, info.Capabilities[i])
			}
		}
		for _, capability := range info.Capabilities {
			if !slices.Contains(known, capability) {
				t.Errorf("%s: unknown capability %q", info.Name, capability)
			}
		}
		// The image installs the CLI release the adapter is tested against.
		dockerfile, err := os.ReadFile("../../../docker/" + info.Name + "/Dockerfile")
		if err != nil {
			t.Fatal(err)
		}
		if pin := "cli_version=" + info.TestedVersion + ";"; !strings.Contains(string(dockerfile), pin) {
			t.Errorf("%s: docker/%s/Dockerfile does not install the tested version; want %q", info.Name, info.Name, pin)
		}
		row := "| `" + info.Name + "` | " + info.Title + " | " + info.TestedVersion + " | [Support details](" + info.Docs + ") |"
		if !strings.Contains(string(readme), row) {
			t.Errorf("%s: README support row is missing or stale; want %q", info.Name, row)
		}
	}
}
