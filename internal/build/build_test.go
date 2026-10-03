package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danthegoodman1/simplecloud/internal/compose"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func resolve(t *testing.T, dir string) *Spec {
	t.Helper()
	svc := &compose.Service{Name: "api", Build: &compose.Build{Context: ".", Dockerfile: "Dockerfile"}}
	s, err := Resolve(dir, "registry.example.com/team", "myapp", svc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The hash is the tag, so it must be stable for identical content and must change
// when anything the build depends on changes.
func TestHashIsDeterministicAndContentSensitive(t *testing.T) {
	files := map[string]string{
		"Dockerfile":  "FROM alpine\nCOPY . /app\n",
		"app/main.go": "package main\n",
		"app/go.mod":  "module app\n",
	}
	dir := fixture(t, files)
	first := resolve(t, dir)
	if first.Hash != resolve(t, dir).Hash {
		t.Fatal("the same content produced two hashes")
	}
	if !strings.HasPrefix(first.Tag, "registry.example.com/team/myapp-api:sc-") {
		t.Errorf("tag: %s", first.Tag)
	}
	if len(first.Tag) != len("registry.example.com/team/myapp-api:sc-")+12 {
		t.Errorf("the tag should carry 12 hash characters: %s", first.Tag)
	}

	// Touching a context file must change it.
	os.WriteFile(filepath.Join(dir, "app/main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	if resolve(t, dir).Hash == first.Hash {
		t.Error("editing a context file must change the hash")
	}

	// So must the Dockerfile, an arg, and the target.
	dir2 := fixture(t, files)
	os.WriteFile(filepath.Join(dir2, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644)
	if resolve(t, dir2).Hash == first.Hash {
		t.Error("editing the Dockerfile must change the hash")
	}
	withArg := &compose.Service{Name: "api", Build: &compose.Build{
		Context: ".", Dockerfile: "Dockerfile", Args: map[string]string{"V": "2"}}}
	s, _ := Resolve(dir, "r", "p", withArg)
	plain := &compose.Service{Name: "api", Build: &compose.Build{Context: ".", Dockerfile: "Dockerfile"}}
	s2, _ := Resolve(dir, "r", "p", plain)
	if s.Hash == s2.Hash {
		t.Error("a build arg must change the hash")
	}
}

// An ignored file must not change the hash, or every deploy would rebuild.
func TestDockerignoreExcludesFiles(t *testing.T) {
	files := map[string]string{
		"Dockerfile":    "FROM alpine\n",
		".dockerignore": "node_modules\n*.log\nbuild/\n",
		"src/index.js":  "console.log(1)\n",
	}
	dir := fixture(t, files)
	before := resolve(t, dir).Hash

	for _, p := range []string{"node_modules/dep/index.js", "debug.log", "build/out.bin"} {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("ignored"), 0o644)
	}
	if resolve(t, dir).Hash != before {
		t.Error("ignored files must not change the hash")
	}

	// A file that is not ignored must.
	os.WriteFile(filepath.Join(dir, "src/other.js"), []byte("x"), 0o644)
	if resolve(t, dir).Hash == before {
		t.Error("a tracked file must change the hash")
	}
}

func TestImageOverridesDerivedName(t *testing.T) {
	dir := fixture(t, map[string]string{"Dockerfile": "FROM alpine\n"})
	svc := &compose.Service{Name: "api", Image: "ghcr.io/me/custom",
		Build: &compose.Build{Context: ".", Dockerfile: "Dockerfile"}}
	s, err := Resolve(dir, "registry.example.com", "myapp", svc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Tag, "ghcr.io/me/custom:sc-") {
		t.Errorf("image: should name the push target, got %s", s.Tag)
	}
}

func TestMissingDockerfileIsNamed(t *testing.T) {
	dir := fixture(t, map[string]string{"other.txt": "x"})
	svc := &compose.Service{Name: "api", Build: &compose.Build{Context: ".", Dockerfile: "Dockerfile"}}
	if _, err := Resolve(dir, "r", "p", svc); err == nil {
		t.Error("a missing Dockerfile should be an error")
	} else if !strings.Contains(err.Error(), "api") {
		t.Errorf("the error should name the service: %v", err)
	}
}
