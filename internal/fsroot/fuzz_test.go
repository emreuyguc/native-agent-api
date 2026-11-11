package fsroot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/fsroot"
)

// FuzzResolve: an accepted directory is a real directory at or below the allowed root, never a
// sibling with the same prefix and never the target of a link that leaves the root.
func FuzzResolve(f *testing.F) {
	for _, seed := range []string{
		"", ".", "allowed", "allowed/sub", "allowed/../allowed-evil", "allowed-evil", "allowed/out",
		"allowed/out/x", "allowed/sub/../../outside", "allowed//sub/", "allowed/./sub", "allowed/\x00",
		"/etc", "../", "allowed/sub/..", "ALLOWED",
	} {
		f.Add(seed)
	}
	base, err := filepath.EvalSymlinks(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	for _, dir := range []string{"allowed/sub", "allowed-evil", "outside"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			f.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "allowed", "out")); err != nil {
		f.Fatal(err)
	}
	root := filepath.Join(base, "allowed")
	set, err := fsroot.New([]string{root})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, requested string) {
		dir := requested
		if !filepath.IsAbs(dir) {
			dir = base + string(filepath.Separator) + requested
		}
		resolved, err := set.Resolve(dir)
		if err != nil {
			return
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			t.Fatalf("Resolve(%q) = %q, outside %q", dir, resolved, root)
		}
		location, err := filepath.EvalSymlinks(resolved)
		if err != nil || location != resolved {
			t.Fatalf("Resolve(%q) = %q, real location %q (%v)", dir, resolved, location, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			t.Fatalf("Resolve(%q) = %q, not a directory", dir, resolved)
		}
	})
}
