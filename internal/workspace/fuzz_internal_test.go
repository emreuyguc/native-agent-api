package workspace

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzResolve: an accepted path is clean, relative, free of "..", and names a location inside
// the workspace.
func FuzzResolve(f *testing.F) {
	for _, seed := range []string{"a.txt", "dir/a", "../x", "/etc/passwd", "a/../../b", "a\\b", "a\x00b", ".", "./a", "a//b/", "link/x", "..."} {
		f.Add(seed)
	}
	dir := f.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o700); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(dir, "link")); err != nil {
		f.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = root.Close() })
	f.Fuzz(func(t *testing.T, requested string) {
		relative, err := resolve(root, requested, true)
		if err != nil {
			return
		}
		if relative == "" || relative == "." || path.IsAbs(relative) || path.Clean(relative) != relative ||
			relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\x00\\") {
			t.Fatalf("resolve(%q) = %q", requested, relative)
		}
		if relative == "link" || strings.HasPrefix(relative, "link/") {
			t.Fatalf("resolve(%q) passed through a symlink", requested)
		}
	})
}
