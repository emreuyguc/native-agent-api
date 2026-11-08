package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const maxEnvFileBytes = 1 << 20

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var applicationEnvNames = func() map[string]struct{} {
	names := make(map[string]struct{})
	// Every prefixed schema: server settings plus the login/status subset.
	for _, schema := range []reflect.Type{
		reflect.TypeFor[spec](), reflect.TypeFor[interactiveSpec](),
	} {
		for field := range schema.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("env"), ",")
			if name != "" {
				names[EnvPrefix+name] = struct{}{}
			}
		}
	}
	return names
}()

// MergeFile reads an explicit application env file, then overlays processEnv. It does not perform
// interpolation, command substitution, file discovery, or fallback lookup.
func MergeFile(processEnv map[string]string, path string) (map[string]string, error) {
	fileEnv, err := readEnvFile(path)
	if err != nil {
		return nil, err
	}
	merged := maps.Clone(fileEnv)
	maps.Copy(merged, processEnv)

	return merged, nil
}

func readEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, errors.New("env file path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect env file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("env file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("env file permissions %04o expose it to group or other users; use 0600", info.Mode().Perm())
	}
	if info.Size() > maxEnvFileBytes {
		return nil, fmt.Errorf("env file exceeds %d bytes", maxEnvFileBytes)
	}
	file, err := os.Open(path) //nolint:gosec // Explicit operator-supplied configuration path.
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(file, maxEnvFileBytes+1))
	scanner.Buffer(make([]byte, 4096), maxEnvFileBytes)
	for line := 1; scanner.Scan(); line++ {
		name, value, present, err := parseEnvLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("env file line %d: %w", line, err)
		}
		if !present {
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("env file line %d: duplicate variable %s", line, name)
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}

	return values, nil
}

func parseEnvLine(line string) (name, value string, present bool, err error) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	name, raw, ok := strings.Cut(line, "=")
	name = strings.TrimSpace(name)
	if !ok || !envName.MatchString(name) {
		return "", "", false, errors.New("expected NAME=value")
	}
	if !strings.HasPrefix(name, EnvPrefix) {
		return "", "", false, fmt.Errorf("%s is not an application variable (%s* required)", name, EnvPrefix)
	}
	if _, known := applicationEnvNames[name]; !known {
		return "", "", false, fmt.Errorf("unknown application variable %s", name)
	}
	value, err = parseEnvValue(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false, err
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", "", false, errors.New("value contains NUL")
	}

	return name, value, true, nil
}

func parseEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single-quoted value")
		}
		end++
		if err := validateEnvTail(raw[end+1:]); err != nil {
			return "", err
		}
		return raw[1:end], nil
	case '"':
		end := closingDoubleQuote(raw)
		if end < 0 {
			return "", errors.New("unterminated double-quoted value")
		}
		if err := validateEnvTail(raw[end+1:]); err != nil {
			return "", err
		}
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return value, nil
	default:
		for i := 1; i < len(raw); i++ {
			if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
				raw = raw[:i]
				break
			}
		}
		return strings.TrimSpace(raw), nil
	}
}

func closingDoubleQuote(value string) int {
	escaped := false
	for i := 1; i < len(value); i++ {
		switch {
		case escaped:
			escaped = false
		case value[i] == '\\':
			escaped = true
		case value[i] == '"':
			return i
		}
	}
	return -1
}

func validateEnvTail(tail string) error {
	tail = strings.TrimSpace(tail)
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return errors.New("unexpected text after quoted value")
	}
	return nil
}
