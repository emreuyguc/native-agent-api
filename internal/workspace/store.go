// Package workspace manages workspace directories and the file operations exposed over the API.
//
// A managed workspace is a directory directly under the store root, named by its ID. A
// host-directory attachment points at an existing project directory inside an allowed root
// (NATIVE_AGENT_ALLOWED_ROOTS); its record is stored under <root>/.attachments and removing
// the workspace removes only that record. Every path supplied by a client is resolved relative
// to the workspace directory; traversal and symbolic links are rejected so a request can
// never reach outside the workspace.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/fsroot"
	"github.com/emreuyguc/native-agent-api/internal/ids"
)

// Sentinel errors returned by Store. Callers map them to API status codes with errors.Is.
var (
	ErrInvalidID        = errors.New("workspace id is invalid")
	ErrNotFound         = errors.New("not found")
	ErrInvalidPath      = errors.New("file path is invalid")
	ErrFileTooLarge     = errors.New("file exceeds the configured upload limit")
	ErrTooManyEntries   = errors.New("workspace contains too many entries")
	ErrIsDirectory      = errors.New("target is a directory")
	ErrPathConflict     = errors.New("destination already exists")
	ErrInvalidOperation = errors.New("file operation is invalid")
	// ErrAttachmentDisabled is returned when no allowed roots are configured.
	ErrAttachmentDisabled = errors.New("host-directory attachments need NATIVE_AGENT_ALLOWED_ROOTS")
)

// attachmentsDir holds host-directory attachment records; createdDir holds one empty record
// per managed workspace whose modification time is the creation time.
const (
	attachmentsDir = ".attachments"
	createdDir     = ".created"
)

// Info describes a workspace.
type Info struct {
	ID      string `json:"workspace_id"`
	Storage string `json:"storage" enum:"managed,attachment" doc:"managed: a directory the gateway owns; attachment: a host directory."`
	// Attachment is present when Storage is attachment.
	Attachment *Attachment `json:"attachment,omitempty"`
	Times      Times       `json:"times"`
}

// Times are a workspace's timestamps.
type Times struct {
	CreatedAt time.Time `json:"created_at" doc:"When the workspace was created or attached."`
	// LastUsedAt is the latest session activity in the workspace (a run starting, binding its
	// conversation or finishing); absent before the first run.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Attachment is the host directory backing an attached workspace.
type Attachment struct {
	Path  string `json:"path"`
	State string `json:"state" enum:"ready,unavailable" doc:"unavailable: the directory is gone or no longer inside an allowed root."`
}

// Entry describes one file or directory inside a workspace.
type Entry struct {
	Path      string    `json:"path"`
	Directory bool      `json:"directory"`
	Size      int64     `json:"size"`
	MediaType string    `json:"media_type,omitempty"`
	Modified  time.Time `json:"modified_at"`
}

// UploadedFile is the result of a successful upload.
type UploadedFile struct {
	Entry

	SHA256 string `json:"sha256"`
}

// Store owns the workspace root directory.
type Store struct {
	root         string
	roots        *fsroot.Set
	maxFileBytes int64
	maxEntries   int
	// protected are directories an attachment must neither be inside nor contain: gateway
	// state and the agent profile (credentials, settings, history).
	protected []string
	linkMu    sync.Mutex
}

// NewStore creates root (mode 0700) when missing and returns a Store backed by it.
// roots (may be nil) are the directories that can be attached as workspaces.
func NewStore(root string, roots *fsroot.Set, maxFileBytes int64, maxEntries int) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("initialize workspace root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", root)
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}

	return &Store{root: resolved, roots: roots, maxFileBytes: maxFileBytes, maxEntries: maxEntries}, nil
}

// Protect adds directories that attachments must neither be inside nor contain. Call it
// before serving requests.
func (s *Store) Protect(dirs ...string) {
	for _, dir := range dirs {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		s.protected = append(s.protected, filepath.Clean(dir))
	}
}

// overlapsProtected reports whether path is inside or contains the workspace root or a
// protected directory.
func (s *Store) overlapsProtected(path string) bool {
	for _, guarded := range append([]string{s.root}, s.protected...) {
		if fsroot.Within(guarded, path) || fsroot.Within(path, guarded) {
			return true
		}
	}

	return false
}

// Health verifies that the configured storage root is still an accessible directory.
func (s *Store) Health() error {
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("inspect workspace storage: %w", err)
	}
	if !info.IsDir() {
		return errors.New("workspace storage is not a directory")
	}

	return nil
}

// AttachHostDirectory registers an existing directory inside an allowed root. Attaching the
// same directory again returns the existing workspace.
func (s *Store) AttachHostDirectory(path string) (Info, error) {
	if s.roots.Empty() {
		return Info{}, ErrAttachmentDisabled
	}
	resolved, err := s.roots.Resolve(path)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrInvalidPath, err)
	}
	if fsroot.Within(s.root, resolved) {
		return Info{}, fmt.Errorf("%w: managed workspaces cannot be attached", ErrInvalidPath)
	}
	if s.overlapsProtected(resolved) {
		return Info{}, fmt.Errorf("%w: the directory contains or is inside gateway state or the agent profile", ErrInvalidPath)
	}
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.root, attachmentsDir), 0o700); err != nil {
		return Info{}, fmt.Errorf("attach host directory: %w", err)
	}
	for _, info := range s.attachments() {
		if info.Attachment.Path == resolved {
			return info, nil
		}
	}
	for range 5 {
		id := ids.NewWorkspaceID()
		file, err := os.OpenFile(filepath.Join(s.root, attachmentsDir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // id is a freshly generated workspace ID.
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Info{}, fmt.Errorf("attach host directory: %w", err)
		}
		_, err = file.WriteString(resolved)
		if err = errors.Join(err, file.Close()); err != nil {
			return Info{}, fmt.Errorf("attach host directory: %w", err)
		}

		return attachedInfo(id, resolved, true, time.Now().UTC()), nil
	}

	return Info{}, errors.New("attach host directory: unable to allocate a unique id")
}

// Info returns a workspace description.
func (s *Store) Info(id string) (Info, error) {
	if !ids.ValidWorkspaceID(id) {
		return Info{}, ErrInvalidID
	}
	dir := filepath.Join(s.root, id)
	if entry, err := os.Lstat(dir); err == nil && entry.IsDir() && entry.Mode()&fs.ModeSymlink == 0 {
		return s.managedInfo(id, entry), nil
	}
	if info, ok := s.attachment(id); ok {
		return info, nil
	}

	return Info{}, fmt.Errorf("workspace %w", ErrNotFound)
}

// Workspaces lists managed and attached workspaces.
func (s *Store) Workspaces() ([]Info, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && ids.ValidWorkspaceID(entry.Name()) {
			if stat, err := entry.Info(); err == nil {
				infos = append(infos, s.managedInfo(entry.Name(), stat))
			}
		}
	}
	infos = append(infos, s.attachments()...)
	slices.SortFunc(infos, func(a, b Info) int { return strings.Compare(a.ID, b.ID) })

	return infos, nil
}

// attachments returns every attachment record, including currently unavailable targets.
func (s *Store) attachments() []Info {
	entries, _ := os.ReadDir(filepath.Join(s.root, attachmentsDir))
	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if info, ok := s.attachment(entry.Name()); ok {
			infos = append(infos, info)
		}
	}

	return infos
}

func (s *Store) attachment(id string) (Info, bool) {
	if !ids.ValidWorkspaceID(id) {
		return Info{}, false
	}
	record := filepath.Join(s.root, attachmentsDir, id)
	stat, err := os.Lstat(record)
	if err != nil {
		return Info{}, false
	}
	data, err := os.ReadFile(record) //nolint:gosec // id passed ValidWorkspaceID above.
	if err != nil || len(data) == 0 || len(data) > 4096 {
		return Info{}, false
	}
	path := string(data)
	resolved, err := s.roots.Resolve(path)
	ready := err == nil && !fsroot.Within(s.root, resolved)
	if ready {
		path = resolved
	}

	return attachedInfo(id, path, ready, stat.ModTime().UTC()), true
}

func attachedInfo(id, path string, ready bool, created time.Time) Info {
	state := "unavailable"
	if ready {
		state = "ready"
	}

	return Info{ID: id, Storage: "attachment", Attachment: &Attachment{Path: path, State: state}, Times: Times{CreatedAt: created}}
}

// managedInfo describes a managed workspace. One created before creation records were kept
// reports its directory's modification time.
func (s *Store) managedInfo(id string, dir fs.FileInfo) Info {
	created := dir.ModTime()
	if record, err := os.Lstat(filepath.Join(s.root, createdDir, id)); err == nil {
		created = record.ModTime()
	}

	return Info{ID: id, Storage: "managed", Times: Times{CreatedAt: created.UTC()}}
}

// linkTarget reads a link and re-validates it against the allowed roots on every use.
func (s *Store) linkTarget(id string) (string, bool) {
	info, ok := s.attachment(id)
	// Recorded attachments are re-checked so a changed profile or root cannot expose them.
	if !ok || info.Attachment.State != "ready" || s.overlapsProtected(info.Attachment.Path) {
		return "", false
	}

	return info.Attachment.Path, true
}

// Create allocates a new empty workspace and returns its ID.
func (s *Store) Create() (string, error) {
	if err := os.MkdirAll(filepath.Join(s.root, createdDir), 0o700); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	for range 5 {
		id := ids.NewWorkspaceID()
		err := os.Mkdir(filepath.Join(s.root, id), 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err == nil {
			if err = os.WriteFile(filepath.Join(s.root, createdDir, id), nil, 0o600); err != nil {
				_ = os.Remove(filepath.Join(s.root, id))
			}
		}
		if err != nil {
			return "", fmt.Errorf("create workspace: %w", err)
		}

		return id, nil
	}

	return "", errors.New("create workspace: unable to allocate a unique id")
}

// Dir returns the absolute directory of an existing workspace.
func (s *Store) Dir(id string) (string, error) {
	if !ids.ValidWorkspaceID(id) {
		return "", ErrInvalidID
	}
	dir := filepath.Join(s.root, id)
	info, err := os.Lstat(dir)
	if err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		return dir, nil
	}
	if target, ok := s.linkTarget(id); ok {
		return target, nil
	}

	return "", fmt.Errorf("workspace %w", ErrNotFound)
}

// Remove deletes a managed workspace with all of its contents, or only the record of an
// attached workspace (the project directory is never touched).
func (s *Store) Remove(id string) error {
	if !ids.ValidWorkspaceID(id) {
		return ErrInvalidID
	}
	if _, attached := s.attachment(id); attached {
		if err := os.Remove(filepath.Join(s.root, attachmentsDir, id)); err != nil {
			return fmt.Errorf("detach workspace: %w", err)
		}
		return nil
	}
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}
	if err := os.Remove(filepath.Join(s.root, createdDir, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove workspace: %w", err)
	}

	return nil
}

// List returns every entry in the workspace sorted by path. Symbolic links are skipped.
func (s *Store) List(id string) ([]Entry, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries := make([]Entry, 0)
	err = fs.WalkDir(root.FS(), ".", func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == "." || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if len(entries) >= s.maxEntries {
			return ErrTooManyEntries
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := Entry{Path: current, Directory: d.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC()}
		if !entry.Directory {
			entry.MediaType = MediaType(entry.Path)
		}
		entries = append(entries, entry)

		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTooManyEntries) {
			return nil, err
		}

		return nil, fmt.Errorf("list workspace: %w", err)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })

	return entries, nil
}

// Upload streams source into requestedPath atomically (temporary file + rename) and
// returns the stored file metadata together with its SHA-256 digest.
func (s *Store) Upload(id, requestedPath string, source io.Reader) (UploadedFile, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return UploadedFile{}, err
	}
	defer root.Close()
	target, err := resolve(root, requestedPath, true)
	if err != nil {
		return UploadedFile{}, err
	}
	if parent := parentOf(target); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return UploadedFile{}, fmt.Errorf("%w: create upload directory: %w", ErrInvalidPath, err)
		}
	}
	if info, err := root.Lstat(target); err == nil && info.IsDir() {
		return UploadedFile{}, ErrIsDirectory
	}

	temporary := temporaryName(target, ".upload-")
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("create upload file: %w", err)
	}
	defer func() { _ = root.Remove(temporary) }()

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, s.maxFileBytes+1))
	closeErr := file.Close()
	if written > s.maxFileBytes {
		return UploadedFile{}, ErrFileTooLarge
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		return UploadedFile{}, fmt.Errorf("store uploaded file: %w", err)
	}
	if err := root.Rename(temporary, target); err != nil {
		return UploadedFile{}, fmt.Errorf("finalize uploaded file: %w", err)
	}
	info, err := root.Stat(target)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("inspect uploaded file: %w", err)
	}

	return UploadedFile{
		Path: target, Size: info.Size(), MediaType: MediaType(target), Modified: info.ModTime().UTC(),
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// Open returns a regular file for reading. The caller must close it.
func (s *Store) Open(id, requestedPath string) (*os.File, Entry, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return nil, Entry{}, err
	}
	defer root.Close() // The opened file stays valid.
	target, err := resolve(root, requestedPath, false)
	if err != nil {
		return nil, Entry{}, err
	}
	file, info, err := openRegular(root, target)
	if err != nil {
		return nil, Entry{}, fmt.Errorf("file %w", ErrNotFound)
	}

	return file, Entry{Path: target, Size: info.Size(), MediaType: MediaType(target), Modified: info.ModTime().UTC()}, nil
}

// Delete removes a file or directory tree inside the workspace.
func (s *Store) Delete(id, requestedPath string) error {
	root, err := s.openRoot(id)
	if err != nil {
		return err
	}
	defer root.Close()
	target, err := resolve(root, requestedPath, false)
	if err != nil {
		return err
	}
	entries := 0
	if err := fs.WalkDir(root.FS(), target, func(current string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current != target {
			entries++
		}
		if entries > s.maxEntries {
			return ErrTooManyEntries
		}

		return nil
	}); err != nil {
		if errors.Is(err, ErrTooManyEntries) {
			return err
		}
		return fmt.Errorf("inspect workspace path for deletion: %w", err)
	}
	if err := root.RemoveAll(target); err != nil {
		return fmt.Errorf("delete workspace path: %w", err)
	}

	return nil
}
