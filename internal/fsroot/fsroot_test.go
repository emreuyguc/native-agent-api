package fsroot_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/fsroot"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	outside := filepath.Join(base, "secret")
	for _, dir := range []string{filepath.Join(root, "app", "src"), outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := fsroot.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)

	if got, err := set.Resolve(filepath.Join(root, "app", "src")); err != nil || got != filepath.Join(realRoot, "app", "src") {
		t.Fatalf("inside = %q, %v", got, err)
	}
	for _, path := range []string{
		outside, filepath.Join(root, "escape"), filepath.Join(root, "..", "secret"), filepath.Join(root, "file"),
		filepath.Join(root, "missing"), "relative/path", root + "-sibling",
	} {
		if _, err := set.Resolve(path); !errors.Is(err, fsroot.ErrOutside) {
			t.Errorf("Resolve(%q) error = %v", path, err)
		}
	}
	// Extra bases (the workspace) are accepted even without roots.
	if _, err := (*fsroot.Set)(nil).Resolve(filepath.Join(root, "app"), root); err != nil {
		t.Fatalf("extra base: %v", err)
	}
	if _, err := fsroot.New([]string{filepath.Join(base, "missing")}); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := fsroot.New([]string{"relative"}); err == nil {
		t.Fatal("relative root accepted")
	}
}
