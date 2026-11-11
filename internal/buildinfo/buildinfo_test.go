package buildinfo_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/buildinfo"
)

func TestNameMatchesModule(t *testing.T) {
	t.Parallel()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleFile := filepath.Join(filepath.Dir(filepath.Dir(dir)), "go.mod")
	content, err := os.ReadFile(moduleFile)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(content), "\n")
	module, ok := strings.CutPrefix(line, "module ")
	if !ok {
		t.Fatalf("first go.mod line is %q", line)
	}
	if name := filepath.Base(strings.TrimSpace(module)); name != buildinfo.Name {
		t.Fatalf("buildinfo.Name = %q, module name = %q", buildinfo.Name, name)
	}
}

var fromLine = regexp.MustCompile(`(?m)^FROM (?:--platform=\S+ )?(\S+)`)

// TestDockerBaseImagesArePinnedAlike: the base images are constants of the distribution, written
// in each Dockerfile. Every Dockerfile uses the same tag and digest per image, and the Go image is
// at least the Go version of go.mod.
func TestDockerBaseImagesArePinnedAlike(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	files, err := filepath.Glob(filepath.Join(root, "docker", "*", "Dockerfile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Dockerfiles: %v", err)
	}
	pins := map[string]string{} // repository -> reference
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range fromLine.FindAllStringSubmatch(string(content), -1) {
			reference := match[1]
			repository, _, _ := strings.Cut(reference, ":")
			if !strings.Contains(reference, "@sha256:") {
				t.Errorf("%s: %s is not pinned by digest", file, reference)
			}
			if known, ok := pins[repository]; ok && known != reference {
				t.Errorf("%s: %s differs from %s in another Dockerfile", file, reference, known)
			}
			pins[repository] = reference
		}
	}
	if pins["golang"] == "" || pins["alpine"] == "" {
		t.Fatalf("expected golang and alpine base images, found %v", pins)
	}

	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	wanted := regexp.MustCompile(`(?m)^go (\d+)\.(\d+)\.(\d+)$`).FindStringSubmatch(string(module))
	image := regexp.MustCompile(`^golang:(\d+)\.(\d+)\.(\d+)-`).FindStringSubmatch(pins["golang"])
	if wanted == nil || image == nil {
		t.Fatalf("cannot read the Go version of go.mod or of %s", pins["golang"])
	}
	for i := 1; i <= 3; i++ {
		have, _ := strconv.Atoi(image[i])
		want, _ := strconv.Atoi(wanted[i])
		if have > want {
			break
		}
		if have < want {
			t.Fatalf("%s is older than go.mod (go %s.%s.%s)", pins["golang"], wanted[1], wanted[2], wanted[3])
		}
	}
}
