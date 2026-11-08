package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

// ConflictPolicy defines what a file operation does when its destination exists.
type ConflictPolicy string

const (
	ConflictError   ConflictPolicy = "error"
	ConflictReplace ConflictPolicy = "replace"
)

// CreateDirectory creates one directory. Its parent must already exist.
func (s *Store) CreateDirectory(id, requestedPath string, conflict ConflictPolicy) (Entry, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	target, err := operationDestination(root, requestedPath, conflict)
	if err != nil {
		return Entry{}, err
	}
	temporary := temporaryName(target, ".mkdir-")
	if err := root.Mkdir(temporary, 0o700); err != nil {
		return Entry{}, fmt.Errorf("prepare directory: %w", err)
	}
	defer func() { _ = root.RemoveAll(temporary) }()
	if err := installPath(root, temporary, target, conflict); err != nil {
		return Entry{}, err
	}

	return inspectEntry(root, target)
}

// Copy copies a regular file or directory tree. Symbolic links and special files are rejected.
func (s *Store) Copy(id, sourcePath, destinationPath string, conflict ConflictPolicy) (Entry, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	source, err := resolve(root, sourcePath, false)
	if err != nil {
		return Entry{}, err
	}
	target, err := operationDestination(root, destinationPath, conflict)
	if err != nil {
		return Entry{}, err
	}
	if pathsOverlap(source, target) {
		return Entry{}, fmt.Errorf("%w: source and destination must not contain each other", ErrInvalidPath)
	}
	temporary := temporaryName(target, ".copy-")
	defer func() { _ = root.RemoveAll(temporary) }()
	if err := s.copyPath(root, source, temporary); err != nil {
		return Entry{}, err
	}
	if err := installPath(root, temporary, target, conflict); err != nil {
		return Entry{}, err
	}

	return inspectEntry(root, target)
}

// Move renames a regular file or directory inside one workspace.
func (s *Store) Move(id, sourcePath, destinationPath string, conflict ConflictPolicy) (Entry, error) {
	root, err := s.openRoot(id)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	source, err := resolve(root, sourcePath, false)
	if err != nil {
		return Entry{}, err
	}
	target, err := operationDestination(root, destinationPath, conflict)
	if err != nil {
		return Entry{}, err
	}
	if pathsOverlap(source, target) {
		return Entry{}, fmt.Errorf("%w: source and destination must not contain each other", ErrInvalidPath)
	}
	if err := installPath(root, source, target, conflict); err != nil {
		return Entry{}, err
	}

	return inspectEntry(root, target)
}

func operationDestination(root *os.Root, requestedPath string, conflict ConflictPolicy) (string, error) {
	if conflict != ConflictError && conflict != ConflictReplace {
		return "", fmt.Errorf("%w: conflict must be error or replace", ErrInvalidOperation)
	}
	target, err := resolve(root, requestedPath, true)
	if err != nil {
		return "", err
	}
	if info, err := root.Stat(path.Dir(target)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: destination parent must exist", ErrInvalidPath)
	}

	return target, nil
}

func (s *Store) copyPath(root *os.Root, source, destination string) error {
	info, err := root.Lstat(source)
	if err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	switch {
	case info.Mode().IsRegular():
		return s.copyRegularFile(root, source, destination)
	case !info.IsDir():
		return fmt.Errorf("%w: source must be a regular file or directory", ErrInvalidPath)
	}
	entries := 0

	return fs.WalkDir(root.FS(), source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := destination + current[len(source):]
		if current != source {
			entries++
		}
		switch {
		case entries > s.maxEntries:
			return ErrTooManyEntries
		case entry.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: symbolic links are not allowed", ErrInvalidPath)
		case entry.IsDir():
			return root.Mkdir(target, 0o700)
		case !entry.Type().IsRegular():
			return fmt.Errorf("%w: special files are not allowed", ErrInvalidPath)
		}

		return s.copyRegularFile(root, current, target)
	})
}

func (s *Store) copyRegularFile(root *os.Root, source, destination string) error {
	input, info, err := openRegular(root, source)
	if err != nil {
		return err
	}
	defer input.Close()
	if info.Size() > s.maxFileBytes {
		return ErrFileTooLarge
	}
	output, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create copy destination: %w", err)
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, s.maxFileBytes+1))
	closeErr := output.Close()
	if written > s.maxFileBytes {
		return ErrFileTooLarge
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}

	return nil
}

func installPath(root *os.Root, source, target string, conflict ConflictPolicy) error {
	targetInfo, err := root.Lstat(target)
	switch {
	case err == nil && conflict == ConflictError:
		return ErrPathConflict
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspect destination: %w", err)
	case errors.Is(err, fs.ErrNotExist):
		if err := root.Rename(source, target); err != nil {
			return fmt.Errorf("install destination: %w", err)
		}
		return nil
	}
	sourceInfo, sourceErr := root.Lstat(source)
	if sourceErr != nil {
		return fmt.Errorf("inspect source: %w", sourceErr)
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return fmt.Errorf("%w: source and destination identify the same file", ErrInvalidPath)
	}

	backup := temporaryName(target, ".replace-")
	if err := root.Rename(target, backup); err != nil {
		return fmt.Errorf("stage replaced destination: %w", err)
	}
	if err := root.Rename(source, target); err != nil {
		rollbackErr := root.Rename(backup, target)
		return errors.Join(fmt.Errorf("install replacement: %w", err), rollbackErr)
	}
	if err := root.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove replaced destination: %w", err)
	}

	return nil
}

func inspectEntry(root *os.Root, target string) (Entry, error) {
	info, err := root.Stat(target)
	if err != nil {
		return Entry{}, fmt.Errorf("inspect operation result: %w", err)
	}
	entry := Entry{Path: target, Directory: info.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC()}
	if !entry.Directory {
		entry.MediaType = MediaType(target)
	}

	return entry, nil
}
