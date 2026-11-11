package claude_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/agent"
	"github.com/emreuyguc/native-agent-api/internal/agent/claude"
)

// FuzzParseOptions: options that ParseOptions accepts never inject a flag (docs/security.md F-3).
// The CLI reads every value after a variadic flag until the next value that starts with "-", so
// each list must reach the CLI whole, with no entry that the CLI would read as a flag.
func FuzzParseOptions(f *testing.F) {
	for _, seed := range []string{
		`{"allowed_tools":["Read","Bash(git *)"]}`,
		`{"allowed_tools":["Read","--settings={}"]}`,
		`{"disallowed_tools":["-p"]}`,
		`{"tools":["--mcp-config=x"]}`,
		`{"betas":["context-1m"],"files":["file_abc:a.txt"]}`,
		`{"files":["file_abc:-x"],"session_name":"-name"}`,
		`{"allowed_tools":[" --x"],"betas":["-b"]}`,
	} {
		f.Add(seed)
	}
	variadic := map[string]func(claude.RequestOptions) []string{
		"--allowed-tools":    func(o claude.RequestOptions) []string { return o.AllowedTools },
		"--disallowed-tools": func(o claude.RequestOptions) []string { return o.DisallowedTools },
		"--betas":            func(o claude.RequestOptions) []string { return o.Betas },
		"--file":             func(o claude.RequestOptions) []string { return o.Files },
	}
	f.Fuzz(func(t *testing.T, options string) {
		req := agent.Request{
			WorkspaceID: strings.Repeat("a", 32), SessionID: testSessionID, SessionMode: agent.SessionNew,
			Permissions: agent.PermissionsAsk, Options: json.RawMessage(options),
		}
		parsed, err := claude.ParseOptions(&req)
		if err != nil {
			return
		}
		args := claude.Arguments(req, parsed, agent.SandboxCLI, claude.Files{EmptyMCP: "/tmp/mcp.json"})
		for i, arg := range args {
			list, ok := variadic[arg]
			if !ok {
				continue
			}
			read := 0
			for _, value := range args[i+1:] {
				if strings.HasPrefix(value, "-") {
					break
				}
				read++
			}
			if want := len(list(parsed)); read < want {
				t.Fatalf("%s reads %d of %d values; the rest become flags: %q", arg, read, want, args[i+1:])
			}
		}
		for _, value := range []string{parsed.SessionName, parsed.Agent, parsed.FallbackModel, parsed.Autocompact} {
			if strings.HasPrefix(value, "-") {
				t.Fatalf("accepted a value that starts with -: %q", value)
			}
		}
	})
}
