package workspace_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/fsroot"
	"github.com/emreuyguc/native-agent-api/internal/workspace"
)

func newStore(t *testing.T) *workspace.Store {
	t.Helper()
	store, err := workspace.NewStore(filepath.Join(t.TempDir(), "workspaces"), nil, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

func TestFileLifecycle(t *testing.T) {
	t.Parallel()
	store := newStore(t)

	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := store.Upload(id, "output/result.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Path != "output/result.txt" || uploaded.Size != 5 || len(uploaded.SHA256) != 64 {
		t.Fatalf("unexpected upload %+v", uploaded)
	}
	if _, err := store.CreateDirectory(id, "arşiv", workspace.ConflictError); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Copy(id, "output/result.txt", "arşiv/result.txt", workspace.ConflictError); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Copy(id, "output/result.txt", "arşiv/result.txt", workspace.ConflictError); !errors.Is(err, workspace.ErrPathConflict) {
		t.Fatalf("copy conflict error = %v", err)
	}
	if _, err := store.Move(id, "arşiv/result.txt", "arşiv/final.txt", workspace.ConflictError); err != nil {
		t.Fatal(err)
	}

	entries, err := store.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || entries[0].Path != "arşiv" || !entries[0].Directory || entries[1].Path != "arşiv/final.txt" || entries[3].MediaType != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected listing %+v", entries)
	}

	file, _, err := store.Open(id, "output/result.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(file)
	_ = file.Close()
	if string(body) != "hello" {
		t.Fatalf("read %q", body)
	}

	if err := store.Delete(id, "output"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Dir(id); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("Dir after Remove = %v", err)
	}
}

func TestUploadRejectsUnsafePaths(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.Dir(id)
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}

	for _, unsafe := range []string{"", "../escape.txt", "/etc/passwd", "a/../../b", "a\\b", "nul\x00byte", "symlink/escape.txt"} {
		if _, err := store.Upload(id, unsafe, strings.NewReader("bad")); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("Upload(%q) error = %v, want ErrInvalidPath", unsafe, err)
		}
	}
	if _, err := store.CreateDirectory(id, "tree", workspace.ConflictError); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Copy(id, "tree", "tree/nested", workspace.ConflictError); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("recursive copy error = %v, want ErrInvalidPath", err)
	}
}

func TestUploadEnforcesSizeLimit(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Upload(id, "big.bin", strings.NewReader(strings.Repeat("x", 1<<20+1)))
	if !errors.Is(err, workspace.ErrFileTooLarge) {
		t.Fatalf("Upload error = %v, want ErrFileTooLarge", err)
	}
	if entries, _ := store.List(id); len(entries) != 0 {
		t.Fatalf("partial upload left entries: %+v", entries)
	}
}

func TestFileBoundariesAndAtomicConcurrentUpload(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.Dir(id)
	external := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(dir, "hardlink.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(dir, "hardlink-alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(id, "hardlink.txt"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("hardlink download error = %v, want ErrNotFound", err)
	}
	if _, err := store.Copy(id, "hardlink.txt", "copied.txt", workspace.ConflictError); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("hardlink copy error = %v, want ErrInvalidPath", err)
	}
	if _, err := store.Move(id, "hardlink.txt", "hardlink-alias.txt", workspace.ConflictReplace); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("same-inode move error = %v, want ErrInvalidPath", err)
	}
	interrupted := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(errors.New("read interrupted")))
	if _, err := store.Upload(id, "interrupted/file.txt", interrupted); err == nil {
		t.Fatal("interrupted upload succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "interrupted", "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted upload left a file: %v", err)
	}

	contents := []string{strings.Repeat("a", 64<<10), strings.Repeat("b", 64<<10)}
	var wait sync.WaitGroup
	errs := make(chan error, len(contents))
	for _, content := range contents {
		wait.Go(func() {
			_, err := store.Upload(id, "atomic.txt", strings.NewReader(content))
			errs <- err
		})
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	file, _, err := store.Open(id, "atomic.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || (string(data) != contents[0] && string(data) != contents[1]) {
		t.Fatalf("concurrent upload produced partial content: size=%d err=%v", len(data), err)
	}

	large := filepath.Join(dir, "large")
	if err := os.Mkdir(large, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 101 {
		name := filepath.Join(large, fmt.Sprintf("%03d", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.List(id); !errors.Is(err, workspace.ErrTooManyEntries) {
		t.Fatalf("large tree list error = %v, want ErrTooManyEntries", err)
	}
	if err := store.Delete(id, "large"); !errors.Is(err, workspace.ErrTooManyEntries) {
		t.Fatalf("large tree delete error = %v, want ErrTooManyEntries", err)
	}
}

func TestDirRejectsInvalidID(t *testing.T) {
	t.Parallel()
	if _, err := newStore(t).Dir("../x"); !errors.Is(err, workspace.ErrInvalidID) {
		t.Fatalf("Dir error = %v", err)
	}
}

func TestLinkedWorkspaces(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	app := filepath.Join(projects, "app")
	if err := os.MkdirAll(app, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := fsroot.New([]string{projects})
	if err != nil {
		t.Fatal(err)
	}
	store, err := workspace.NewStore(filepath.Join(base, "workspaces"), roots, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}

	info, err := store.AttachHostDirectory(app)
	if err != nil || info.Storage != "attachment" || info.Attachment.State != "ready" {
		t.Fatalf("AttachHostDirectory = %+v, %v", info, err)
	}
	if again, _ := store.AttachHostDirectory(app + "/"); again.ID != info.ID {
		t.Fatalf("reattaching created %s, want %s", again.ID, info.ID)
	}
	if dir, err := store.Dir(info.ID); err != nil || dir != info.Attachment.Path {
		t.Fatalf("Dir = %q, %v", dir, err)
	}
	if entries, err := store.List(info.ID); err != nil || len(entries) != 1 || entries[0].Path != "main.go" {
		t.Fatalf("List = %+v, %v", entries, err)
	}
	for _, path := range []string{base, filepath.Join(base, "workspaces"), "relative"} {
		if _, err := store.AttachHostDirectory(path); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("Link(%q) error = %v", path, err)
		}
	}
	all, err := store.Workspaces()
	if err != nil || len(all) != 1 {
		t.Fatalf("Workspaces = %+v, %v", all, err)
	}

	// Removing an attached workspace keeps the project directory.
	if err := store.Remove(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, "main.go")); err != nil {
		t.Fatalf("project file removed: %v", err)
	}
	if _, err := store.Dir(info.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("Dir after detach = %v", err)
	}

	// A link whose target later moves outside the roots stops resolving.
	moved, _ := store.AttachHostDirectory(app)
	if err := os.Rename(app, filepath.Join(base, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Dir(moved.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("stale link resolved: %v", err)
	}
	stale, err := store.Info(moved.ID)
	if err != nil || stale.Attachment == nil || stale.Attachment.State != "unavailable" {
		t.Fatalf("stale attachment = %+v, %v", stale, err)
	}
	all, err = store.Workspaces()
	if err != nil || len(all) != 1 || all[0].Attachment == nil || all[0].Attachment.State != "unavailable" {
		t.Fatalf("listed stale attachment = %+v, %v", all, err)
	}
	if err := store.Remove(moved.ID); err != nil {
		t.Fatalf("detach stale attachment: %v", err)
	}

	noRoots, _ := workspace.NewStore(filepath.Join(base, "other"), nil, 1<<20, 100)
	if _, err := noRoots.AttachHostDirectory(projects); !errors.Is(err, workspace.ErrAttachmentDisabled) {
		t.Fatalf("Link without roots = %v", err)
	}
}

func TestAttachmentsCannotExposeProfileOrGatewayState(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home, profile, state, project := filepath.Join(base, "home"), filepath.Join(base, "home", ".codex"),
		filepath.Join(base, "state"), filepath.Join(base, "home", "project")
	for _, dir := range []string{filepath.Join(profile, "sessions"), filepath.Join(state, "workspaces"), project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := fsroot.New([]string{base})
	if err != nil {
		t.Fatal(err)
	}
	store, err := workspace.NewStore(filepath.Join(state, "workspaces"), roots, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := store.AttachHostDirectory(project)
	if err != nil {
		t.Fatal(err)
	}
	store.Protect(profile)
	for _, path := range []string{home, profile, filepath.Join(profile, "sessions"), state} {
		if _, err := store.AttachHostDirectory(path); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("AttachHostDirectory(%q) error = %v, want ErrInvalidPath", path, err)
		}
	}
	if _, err := store.Dir(attached.ID); err != nil {
		t.Fatalf("unrelated attachment became unavailable: %v", err)
	}
	store.Protect(project) // A recorded attachment that now overlaps is no longer served.
	if _, err := store.Dir(attached.ID); err == nil {
		t.Fatal("overlapping recorded attachment is still served")
	}
}

// TestSpecialFilesNeverBlock: the agent can create a FIFO in its workspace; reading, copying or
// listing it must fail at once instead of waiting for a writer (docs/security.md F-4).
func TestSpecialFilesNeverBlock(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.Dir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	go func() {
		file, _, err := store.Open(id, "pipe")
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	go func() {
		_, err := store.Copy(id, "pipe", "copy", workspace.ConflictError)
		done <- err
	}()
	go func() {
		_, err := store.List(id)
		done <- err
	}()
	for range 3 {
		select {
		case err := <-done:
			_ = err // Open and Copy fail, List may include it; none may block.
		case <-time.After(5 * time.Second):
			t.Fatal("a file operation blocked on a FIFO")
		}
	}
	if file, _, err := store.Open(id, "pipe"); err == nil {
		_ = file.Close()
		t.Fatal("a FIFO was opened as a regular file")
	}
}

// TestSwappedParentCannotEscape: a parent directory replaced by a symlink after validation
// (by the agent, which writes the same workspace) must not let an operation leave it.
func TestSwappedParentCannotEscape(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	id, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.Dir(id)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(filepath.Join(dir, "sub"))
			_ = os.Symlink(outside, filepath.Join(dir, "sub"))
			_ = os.Remove(filepath.Join(dir, "sub"))
			_ = os.Mkdir(filepath.Join(dir, "sub"), 0o700)
			_ = os.WriteFile(filepath.Join(dir, "sub", "secret"), []byte("inside"), 0o600)
		}
	})
	defer func() { close(stop); wg.Wait() }()
	for range 2000 {
		if file, _, err := store.Open(id, "sub/secret"); err == nil {
			content, _ := io.ReadAll(file)
			_ = file.Close()
			if string(content) == "secret" {
				t.Fatal("read a file outside the workspace through a swapped parent")
			}
		}
		_, _ = store.Upload(id, "sub/planted", strings.NewReader("x"))
		_ = store.Delete(id, "sub/secret")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("wrote a file outside the workspace through a swapped parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatal("deleted a file outside the workspace through a swapped parent")
	}
}
