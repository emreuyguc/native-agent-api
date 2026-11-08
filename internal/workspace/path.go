package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Every file operation runs on an *os.Root opened at the workspace directory. The kernel then
// keeps each operation inside it even if a component is swapped for a symbolic link after it was
// checked (the agent writes the same directory concurrently), so a validation race can at most
// reach another path inside the same workspace (docs/security.md F-5).

// openRoot opens the workspace directory of id.
func (s *Store) openRoot(id string) (*os.Root, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace %w", ErrNotFound)
	}

	return root, nil
}

// resolve validates a client supplied slash-separated path and returns it cleaned, relative to
// root. It rejects absolute paths, traversal, NUL bytes, backslashes and any symbolic link on the
// way. With allowMissing, trailing components may not exist yet (uploads).
func resolve(root *os.Root, requested string, allowMissing bool) (string, error) {
	if requested == "" || strings.ContainsAny(requested, "\x00\\") || path.IsAbs(requested) {
		return "", ErrInvalidPath
	}
	relative := path.Clean(requested)
	if relative == "." || !filepath.IsLocal(filepath.FromSlash(relative)) {
		return "", ErrInvalidPath
	}

	parts := strings.Split(relative, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) && allowMissing {
			break
		}
		if err != nil {
			return "", fmt.Errorf("file %w", ErrNotFound)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: symbolic links are not allowed", ErrInvalidPath)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("%w: parent is not a directory", ErrInvalidPath)
		}
	}

	return relative, nil
}

// openRegular opens a single-link regular file for reading. O_NONBLOCK keeps a FIFO from
// blocking the open; such a file is then rejected (docs/security.md F-4).
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("file %w", ErrNotFound)
	}
	info, err := file.Stat()
	if err != nil || !safeRegularFile(info) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: not a single-link regular file", ErrInvalidPath)
	}

	return file, info, nil
}

// safeRegularFile rejects hardlinks because a link created outside the workspace can otherwise
// expose that external inode through an apparently local path.
func safeRegularFile(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)

	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}

// temporaryName returns a fresh hidden name next to target.
func temporaryName(target, prefix string) string {
	var random [8]byte
	_, _ = rand.Read(random[:])
	name := prefix + hex.EncodeToString(random[:])
	if parent := path.Dir(target); parent != "." {
		return parent + "/" + name
	}

	return name
}

// parentOf returns the parent of a cleaned relative path ("." at the top).
func parentOf(name string) string { return path.Dir(name) }

// pathsOverlap reports whether two cleaned relative paths are equal or contain each other.
func pathsOverlap(first, second string) bool {
	return first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/")
}

// MediaType guesses the MIME type from the file extension.
func MediaType(filePath string) string {
	if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(filePath))); detected != "" {
		return detected
	}

	return "application/octet-stream"
}
