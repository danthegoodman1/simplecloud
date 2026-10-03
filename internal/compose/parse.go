package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Filenames are tried in order, matching Docker Compose.
var Filenames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// FindFile walks up from dir to the nearest Compose file, the way git finds a
// repository root, so commands work from a subdirectory.
func FindFile(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		for _, n := range Filenames {
			p := filepath.Join(abs, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no Compose file found in %s or any parent directory (looked for %s)", dir, strings.Join(Filenames, ", "))
		}
		abs = parent
	}
}

// Error is a parse or validation failure. Fix carries the remedy, because an
// error that only says "unsupported" leaves the author to guess.
type Error struct {
	Service string
	Key     string
	Problem string
	Fix     string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Service != "" {
		b.WriteString(e.Service + ": ")
	}
	if e.Key != "" {
		b.WriteString(e.Key + ": ")
	}
	b.WriteString(e.Problem)
	if e.Fix != "" {
		b.WriteString("\n  " + e.Fix)
	}
	return b.String()
}

// Errors collects every problem in a file so one run reports all of them.
type Errors []*Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "\n")
}

var topLevelKeys = map[string]bool{
	"services": true, "volumes": true, "name": true, "version": true,
}

var serviceKeys = map[string]bool{
	"image": true, "build": true, "command": true, "environment": true,
	"env_file": true, "ports": true, "volumes": true, "healthcheck": true,
	"depends_on": true, "profiles": true, "deploy": true,
}

// unsupported maps a rejected service key to the reason and the remedy.
var unsupported = map[string][2]string{
	"restart":        {"restart policies are not honored", "The agent supervises the application and restarts it; remove this key."},
	"networks":       {"custom networks are not supported", "Every project gets one private network and services reach each other by name. Remove this key."},
	"network_mode":   {"network modes are not supported", "Remove this key; the project network is managed for you."},
	"configs":        {"configs are not supported", "Use environment, env_file, or x-simplecloud-sync for a config directory."},
	"secrets":        {"secrets are not supported", "Use x-simplecloud-env-file, kept outside the repository."},
	"extends":        {"extends is not supported", "Inline the service definition."},
	"links":          {"links are not supported", "Services reach each other by name already; remove this key."},
	"privileged":     {"privileged is not supported", "A sandbox is a microVM and already has full capabilities inside itself; remove this key."},
	"cap_add":        {"cap_add is not supported", "A sandbox already has full capabilities inside itself; remove this key."},
	"devices":        {"devices are not supported", "Remove this key."},
	"pid":            {"pid namespaces are not supported", "Remove this key."},
	"user":           {"user is taken from the image", "Set USER in the image instead."},
	"container_name": {"container_name is not supported", "A slot is named <service>-<n> and cannot be renamed."},
}

type rawFile struct {
	Name     string               `yaml:"name"`
	Services map[string]yaml.Node `yaml:"services"`
	Volumes  map[string]yaml.Node `yaml:"volumes"`
}

// Load reads and validates a Compose file.
func Load(file string) (*Project, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, fmt.Errorf("%s: file is empty", file)
	}
	doc := root.Content[0]

	var errs Errors
	dir := filepath.Dir(file)
	p := &Project{
		Dir:     dir,
		File:    file,
		Name:    filepath.Base(dir),
		Volumes: map[string]Volume{},
	}

	// Top level: reject unknown keys, accept any x- extension.
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i].Value
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if !topLevelKeys[key] {
			errs = append(errs, &Error{Key: key, Problem: "unsupported top-level key",
				Fix: "Supported top-level keys are services, volumes, and name."})
		}
	}

	var f rawFile
	if err := doc.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if f.Name != "" {
		p.Name = f.Name
	}
	if n, ok := extString(doc, "x-simplecloud-name"); ok && n != "" {
		p.Name = n
	}
	for name := range f.Volumes {
		p.Volumes[name] = Volume{Name: name}
	}
	if len(f.Services) == 0 {
		errs = append(errs, &Error{Problem: "no services defined"})
	}

	names := make([]string, 0, len(f.Services))
	for n := range f.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		node := f.Services[n]
		svc, serr := parseService(n, &node, p)
		errs = append(errs, serr...)
		if svc != nil {
			p.Services = append(p.Services, svc)
		}
	}

	// Reserved names: a service called db-1 would collide with a slot of db.
	known := map[string]bool{}
	for _, s := range p.Services {
		known[s.Name] = true
	}
	for _, s := range p.Services {
		if base, idx, ok := splitSlotName(s.Name); ok && known[base] {
			errs = append(errs, &Error{Service: s.Name,
				Problem: fmt.Sprintf("name collides with replica slot %d of service %q", idx, base),
				Fix:     "Rename this service; slot names are generated as <service>-<n>."})
		}
	}
	for _, s := range p.Services {
		for _, m := range s.Mounts {
			if _, ok := p.Volumes[m.Volume]; !ok {
				errs = append(errs, &Error{Service: s.Name, Key: "volumes",
					Problem: fmt.Sprintf("named volume %q is not declared", m.Volume),
					Fix:     "Add it under the top-level volumes: key."})
			}
		}
		for _, d := range s.DependsOn {
			if !known[d] {
				errs = append(errs, &Error{Service: s.Name, Key: "depends_on",
					Problem: fmt.Sprintf("unknown service %q", d)})
			}
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return p, nil
}

func splitSlotName(name string) (string, int, bool) {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil || n < 1 {
		return "", 0, false
	}
	return name[:i], n, true
}

// ActiveServices returns the services to deploy: those with no profiles, plus
// those in an active profile. The cloud profile is active here and inactive under
// `docker compose up`, which is how a service exists only when deployed.
func (p *Project) ActiveServices(extraProfiles []string) []*Service {
	active := map[string]bool{CloudProfile: true}
	for _, pr := range extraProfiles {
		active[pr] = true
	}
	var out []*Service
	for _, s := range p.Services {
		if len(s.Profiles) == 0 {
			out = append(out, s)
			continue
		}
		for _, pr := range s.Profiles {
			if active[pr] {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func (p *Project) Service(name string) *Service {
	for _, s := range p.Services {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func extString(node *yaml.Node, key string) (string, bool) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value, true
		}
	}
	return "", false
}

func parseDuration(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	return time.ParseDuration(v)
}
