// Package build produces images from a Compose build: section.
//
// Archil neither builds images nor hosts a registry, so builds happen on the
// operator's machine and push to a registry they control.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danthegoodman1/simplecloud/internal/compose"
)

// Platform is the only architecture sandboxes run.
const Platform = "linux/amd64"

// Spec is one service's build, resolved against the project directory.
type Spec struct {
	Service    string
	Context    string
	Dockerfile string
	Args       map[string]string
	Target     string
	// Tag is derived from the content hash, so it is immutable: a push is
	// idempotent and a rollback is a tag that already exists.
	Tag  string
	Hash string
}

// Resolve computes the content hash and the tag it implies.
//
// Change detection is a content hash rather than a registry round trip, and the
// hash is the tag. The same hash also decides whether a service needs
// redeploying, which is what makes `up` on an untouched project do nothing.
func Resolve(projectDir, registry, project string, svc *compose.Service) (*Spec, error) {
	if svc.Build == nil {
		return nil, fmt.Errorf("%s has no build section", svc.Name)
	}
	ctxDir := svc.Build.Context
	if !filepath.IsAbs(ctxDir) {
		ctxDir = filepath.Join(projectDir, ctxDir)
	}
	dockerfile := svc.Build.Dockerfile
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(ctxDir, dockerfile)
	}
	if _, err := os.Stat(dockerfile); err != nil {
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}
	s := &Spec{
		Service: svc.Name, Context: ctxDir, Dockerfile: dockerfile,
		Args: svc.Build.Args, Target: svc.Build.Target,
	}
	hash, err := s.contentHash()
	if err != nil {
		return nil, err
	}
	s.Hash = hash
	name := svc.Image
	if name == "" {
		name = strings.TrimSuffix(registry, "/") + "/" + project + "-" + svc.Name
	}
	s.Tag = fmt.Sprintf("%s:sc-%s", name, hash[:12])
	return s, nil
}

// contentHash covers the Dockerfile, the context after .dockerignore, the build
// args, the target, and the platform — the same inputs a build cache keys on.
func (s *Spec) contentHash() (string, error) {
	ignore, err := loadDockerignore(s.Context)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "platform=%s\ntarget=%s\n", Platform, s.Target)
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "arg=%s=%s\n", k, s.Args[k])
	}
	df, err := os.Open(s.Dockerfile)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "dockerfile\n")
	if _, err := io.Copy(h, df); err != nil {
		df.Close()
		return "", err
	}
	df.Close()

	var files []string
	err = filepath.Walk(s.Context, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(s.Context, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignore.match(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	// Sorted, so the hash does not depend on directory order.
	sort.Strings(files)
	for _, rel := range files {
		full := filepath.Join(s.Context, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "file=%s mode=%o size=%d\n", rel, info.Mode().Perm(), info.Size())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "link=%s\n", target)
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Build builds and pushes the image. Pushing uses the Docker credential store, so
// a registry the operator has already logged into needs no configuration here.
func (s *Spec) Build(out io.Writer) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed, and %s uses build:.\n"+
			"  Install Docker with buildx; amd64 emulation is needed on an arm64 machine", s.Service)
	}
	args := []string{"buildx", "build",
		"--platform", Platform,
		"--file", s.Dockerfile,
		"--tag", s.Tag,
		"--push",
	}
	if s.Target != "" {
		args = append(args, "--target", s.Target)
	}
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+s.Args[k])
	}
	args = append(args, s.Context)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s: %w", s.Service, err)
	}
	return nil
}

// ignorePatterns is a deliberately small .dockerignore matcher: prefix and suffix
// globs, which is what real ignore files use.
type ignorePatterns struct{ patterns []string }

func loadDockerignore(dir string) (*ignorePatterns, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if os.IsNotExist(err) {
		return &ignorePatterns{}, nil
	}
	if err != nil {
		return nil, err
	}
	ip := &ignorePatterns{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ip.patterns = append(ip.patterns, strings.TrimPrefix(filepath.ToSlash(line), "./"))
	}
	return ip, nil
}

func (ip *ignorePatterns) match(rel string) bool {
	for _, p := range ip.patterns {
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		// A directory pattern covers everything under it.
		if strings.HasPrefix(rel, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
		if ok, _ := filepath.Match(p, filepath.Base(rel)); ok && !strings.Contains(p, "/") {
			return true
		}
	}
	return false
}
