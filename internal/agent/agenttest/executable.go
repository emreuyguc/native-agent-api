package agenttest

import (
	"os"
	"syscall"
	"testing"
)

// WriteExecutable writes a script that a test then runs as a fake CLI.
//
// It holds syscall.ForkLock while the file is open for writing. Without it, a parallel test can
// fork in that moment; the child keeps the write descriptor until it calls exec, and running
// the script fails on Linux with "text file busy" (ETXTBSY).
func WriteExecutable(t testing.TB, path, content string) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(content), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}
