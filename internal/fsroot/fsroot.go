// Package fsroot decides which host directories the gateway may hand to an agent.
//
// The operator lists allowed roots once at startup (NATIVE_AGENT_ALLOWED_ROOTS). A path is
// accepted when its real location (after resolving symbolic links) is a directory at or
// below one of the roots, so a link inside a root cannot point the agent elsewhere.
package fsroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside is returned for paths that are not inside an allowed root.
var ErrOutside = errors.New("path is not inside an allowed root")

// Set is an immutable list of allowed roots.
type Set struct {
	roots []string
}

// New resolves roots to real directories. Every root must exist.
func New(roots []string) (*Set, error) {
	set := &Set{}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("allowed root %q must be an absolute path", root)
		}
		resolved, err := realDir(root)
		if err != nil {
			return nil, fmt.Errorf("allowed root %q: %w", root, err)
		}
		set.roots = append(set.roots, resolved)
	}

	return set, nil
}

// Roots returns the resolved roots.
func (s *Set) Roots() []string { return append([]string(nil), s.roots...) }

// Empty reports whether no roots are configured.
func (s *Set) Empty() bool { return s == nil || len(s.roots) == 0 }

// Resolve returns the real path of dir if it is a directory inside an allowed root or
// inside one of the extra base directories (e.g. the run's workspace).
func (s *Set) Resolve(dir string, extra ...string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %q must be absolute", ErrOutside, dir)
	}
	resolved, err := realDir(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOutside, err)
	}
	var bases []string
	if s != nil {
		bases = append(bases, s.roots...)
	}
	for _, base := range extra {
		if resolved, err := realDir(base); err == nil {
			bases = append(bases, resolved)
		}
	}
	for _, base := range bases {
		if Within(base, resolved) {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrOutside, dir)
}

// Within reports whether path is base or below it (both must be clean absolute paths).
func Within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func realDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}

	return resolved, nil
}
