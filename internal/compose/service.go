package compose

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type rawService struct {
	Image       string          `yaml:"image"`
	Build       yaml.Node       `yaml:"build"`
	Command     yaml.Node       `yaml:"command"`
	Environment yaml.Node       `yaml:"environment"`
	EnvFile     yaml.Node       `yaml:"env_file"`
	Ports       []yaml.Node     `yaml:"ports"`
	Expose      []yaml.Node     `yaml:"expose"`
	Volumes     []yaml.Node     `yaml:"volumes"`
	Healthcheck *rawHealthcheck `yaml:"healthcheck"`
	DependsOn   yaml.Node       `yaml:"depends_on"`
	Profiles    []string        `yaml:"profiles"`
	Deploy      *rawDeploy      `yaml:"deploy"`
}

type rawDeploy struct {
	Replicas  *int `yaml:"replicas"`
	Resources *struct {
		Reservations *struct {
			CPUs   string `yaml:"cpus"`
			Memory string `yaml:"memory"`
		} `yaml:"reservations"`
		Limits *struct {
			CPUs   string `yaml:"cpus"`
			Memory string `yaml:"memory"`
		} `yaml:"limits"`
	} `yaml:"resources"`
}

type rawHealthcheck struct {
	Test     yaml.Node `yaml:"test"`
	Interval string    `yaml:"interval"`
	Timeout  string    `yaml:"timeout"`
	Retries  int       `yaml:"retries"`
	Disable  bool      `yaml:"disable"`
}

func parseService(name string, node *yaml.Node, p *Project) (*Service, Errors) {
	var errs Errors
	bad := func(key, problem, fix string) {
		errs = append(errs, &Error{Service: name, Key: key, Problem: problem, Fix: fix})
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if strings.HasPrefix(key, "x-") {
			if !knownExtension(key) {
				bad(key, "unknown extension", "Supported extensions: "+strings.Join(extensionNames(), ", ")+".")
			}
			continue
		}
		if why, ok := unsupported[key]; ok {
			bad(key, why[0], why[1])
			continue
		}
		if !serviceKeys[key] {
			bad(key, "unsupported key", "Remove it, or see the README for the supported subset.")
		}
	}

	var r rawService
	if err := node.Decode(&r); err != nil {
		return nil, append(errs, &Error{Service: name, Problem: err.Error()})
	}

	s := &Service{
		Name:         name,
		Image:        r.Image,
		Environment:  map[string]string{},
		DeployEnv:    map[string]string{},
		Replicas:     DefaultReplicas,
		VCPU:         DefaultVCPU,
		MemMiB:       DefaultMemMiB,
		IdleTimeout:  DefaultIdleTimeout,
		MaxTTL:       DefaultMaxTTL,
		DoorbellPort: DefaultDoorbellPort,
		LogRing:      DefaultLogRing,
		Profiles:     r.Profiles,
	}

	if r.Build.Kind != 0 {
		b, berr := parseBuild(name, &r.Build)
		errs = append(errs, berr...)
		s.Build = b
	}
	if s.Image == "" && s.Build == nil {
		bad("image", "a service needs either image: or build:", "Add image: <reference>, or build: . to build from a Dockerfile.")
	}

	if r.Command.Kind != 0 {
		cmd, cerr := parseStringOrList(name, "command", &r.Command)
		errs = append(errs, cerr...)
		s.Command = cmd
	}
	if r.Environment.Kind != 0 {
		env, eerr := parseEnvMap(name, "environment", &r.Environment)
		errs = append(errs, eerr...)
		s.Environment = env
	}
	if r.EnvFile.Kind != 0 {
		files, ferr := parseStringOrList(name, "env_file", &r.EnvFile)
		errs = append(errs, ferr...)
		s.EnvFiles = files
	}
	if r.DependsOn.Kind != 0 {
		deps, derr := parseDependsOn(name, &r.DependsOn)
		errs = append(errs, derr...)
		s.DependsOn = deps
	}

	for _, pn := range r.Ports {
		port, perr := parsePort(name, &pn)
		errs = append(errs, perr...)
		if port != nil {
			s.Ports = append(s.Ports, *port)
		}
	}
	for _, en := range r.Expose {
		port, perr := parseExposed(name, &en)
		errs = append(errs, perr...)
		if port > 0 {
			s.Expose = append(s.Expose, port)
		}
	}
	for _, vn := range r.Volumes {
		m, verr := parseMount(name, &vn)
		errs = append(errs, verr...)
		if m != nil {
			s.Mounts = append(s.Mounts, *m)
		}
	}
	if r.Healthcheck != nil {
		hc, herr := parseHealthcheck(name, r.Healthcheck)
		errs = append(errs, herr...)
		s.Healthcheck = hc
	}

	if r.Deploy != nil {
		if r.Deploy.Replicas != nil {
			if *r.Deploy.Replicas < 1 {
				bad("deploy.replicas", "must be at least 1", "")
			} else {
				s.Replicas = *r.Deploy.Replicas
			}
		}
		if res := r.Deploy.Resources; res != nil {
			src := res.Reservations
			if src == nil {
				src = res.Limits
			}
			if src != nil {
				if src.CPUs != "" {
					if v, err := strconv.ParseFloat(src.CPUs, 64); err != nil || v <= 0 {
						bad("deploy.resources", fmt.Sprintf("cpus %q is not a positive number", src.CPUs), "")
					} else {
						s.VCPU = int(v + 0.999)
					}
				}
				if src.Memory != "" {
					if mib, err := parseMemory(src.Memory); err != nil {
						bad("deploy.resources", err.Error(), "Use a value like 512M or 2G.")
					} else {
						s.MemMiB = mib
					}
				}
			}
		}
	}

	errs = append(errs, parseExtensions(node, s)...)

	// A service with no reachable port cannot have its activity observed, so
	// inferring that it is idle would be wrong. Default it to keep-awake.
	if !s.keepAwakeSet && len(s.Ports) == 0 && len(s.Expose) == 0 {
		s.KeepAwake = true
	}
	if s.VCPU < 1 || s.VCPU > 32 {
		bad("x-simplecloud-vcpu", fmt.Sprintf("%d vCPU is out of range", s.VCPU), "Choose between 1 and 32.")
	}
	if s.MemMiB < 256 || s.MemMiB > 65536 {
		bad("x-simplecloud-memory", fmt.Sprintf("%d MiB is out of range", s.MemMiB), "Choose between 256 and 65536 MiB.")
	}
	if s.MaxTTL < 60 || s.MaxTTL > 86400 {
		bad("x-simplecloud-max-ttl", fmt.Sprintf("%d seconds is out of range", s.MaxTTL), "Choose between 60 and 86400 seconds; the platform caps it at 24 hours.")
	}
	return s, errs
}

func parseBuild(svc string, node *yaml.Node) (*Build, Errors) {
	var errs Errors
	b := &Build{Context: ".", Dockerfile: "Dockerfile", Args: map[string]string{}}
	if node.Kind == yaml.ScalarNode {
		b.Context = node.Value
		return b, errs
	}
	var raw struct {
		Context    string    `yaml:"context"`
		Dockerfile string    `yaml:"dockerfile"`
		Args       yaml.Node `yaml:"args"`
		Target     string    `yaml:"target"`
		Platform   string    `yaml:"platform"`
	}
	if err := node.Decode(&raw); err != nil {
		return nil, append(errs, &Error{Service: svc, Key: "build", Problem: err.Error()})
	}
	if raw.Context != "" {
		b.Context = raw.Context
	}
	if raw.Dockerfile != "" {
		b.Dockerfile = raw.Dockerfile
	}
	b.Target = raw.Target
	if raw.Platform != "" && raw.Platform != "linux/amd64" {
		errs = append(errs, &Error{Service: svc, Key: "build.platform",
			Problem: fmt.Sprintf("%q cannot run here", raw.Platform),
			Fix:     "Sandboxes are linux/amd64 only. Remove the key, or set linux/amd64."})
	}
	if raw.Args.Kind != 0 {
		args, aerr := parseEnvMap(svc, "build.args", &raw.Args)
		errs = append(errs, aerr...)
		b.Args = args
	}
	return b, errs
}

func parseStringOrList(svc, key string, node *yaml.Node) ([]string, Errors) {
	switch node.Kind {
	case yaml.ScalarNode:
		if key == "command" {
			return splitCommand(node.Value), nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		var out []string
		for _, n := range node.Content {
			out = append(out, n.Value)
		}
		return out, nil
	}
	return nil, Errors{&Error{Service: svc, Key: key, Problem: "want a string or a list"}}
}

// splitCommand splits a shell-form command on whitespace, honoring quotes, which
// is what Compose does for the string form.
func splitCommand(v string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range v {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func parseEnvMap(svc, key string, node *yaml.Node) (map[string]string, Errors) {
	out := map[string]string{}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			out[node.Content[i].Value] = node.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, n := range node.Content {
			k, v, found := strings.Cut(n.Value, "=")
			if !found {
				// Bare NAME takes its value from the environment, as in Compose.
				out[k] = ""
				continue
			}
			out[k] = v
		}
	default:
		return out, Errors{&Error{Service: svc, Key: key, Problem: "want a mapping or a list of KEY=VALUE"}}
	}
	return out, nil
}

func parseDependsOn(svc string, node *yaml.Node) ([]string, Errors) {
	switch node.Kind {
	case yaml.SequenceNode:
		var out []string
		for _, n := range node.Content {
			out = append(out, n.Value)
		}
		return out, nil
	case yaml.MappingNode:
		var out []string
		for i := 0; i+1 < len(node.Content); i += 2 {
			out = append(out, node.Content[i].Value)
		}
		return out, nil
	}
	return nil, Errors{&Error{Service: svc, Key: "depends_on", Problem: "want a list or a mapping"}}
}

func parsePort(svc string, node *yaml.Node) (*Port, Errors) {
	var spec string
	switch node.Kind {
	case yaml.ScalarNode:
		spec = node.Value
	case yaml.MappingNode:
		var raw struct {
			Target    int    `yaml:"target"`
			Published any    `yaml:"published"`
			Protocol  string `yaml:"protocol"`
		}
		if err := node.Decode(&raw); err != nil {
			return nil, Errors{&Error{Service: svc, Key: "ports", Problem: err.Error()}}
		}
		if raw.Protocol != "" && strings.ToLower(raw.Protocol) != "tcp" {
			return nil, Errors{&Error{Service: svc, Key: "ports",
				Problem: fmt.Sprintf("protocol %q is not supported", raw.Protocol),
				Fix:     "Only TCP is carried between services and published."}}
		}
		return classifyPort(svc, raw.Target)
	default:
		return nil, Errors{&Error{Service: svc, Key: "ports", Problem: "want a string or a mapping"}}
	}

	if strings.Contains(spec, "/") {
		base, proto, _ := strings.Cut(spec, "/")
		if strings.ToLower(proto) != "tcp" {
			return nil, Errors{&Error{Service: svc, Key: "ports",
				Problem: fmt.Sprintf("protocol %q in %q is not supported", proto, spec),
				Fix:     "Only TCP is carried between services and published."}}
		}
		spec = base
	}
	if strings.Contains(spec, "-") {
		return nil, Errors{&Error{Service: svc, Key: "ports",
			Problem: fmt.Sprintf("port range %q is not supported", spec),
			Fix:     "List each port separately."}}
	}
	parts := strings.Split(spec, ":")
	container := parts[len(parts)-1]
	n, err := strconv.Atoi(container)
	if err != nil || n < 1 || n > 65535 {
		return nil, Errors{&Error{Service: svc, Key: "ports",
			Problem: fmt.Sprintf("%q is not a valid port", spec)}}
	}
	return classifyPort(svc, n)
}

// classifyPort decides whether a published port is carried through. A datastore
// port is recorded as skipped rather than rejected, so the same Compose file
// deploys here and still maps to the laptop under `docker compose up`.
func classifyPort(svc string, n int) (*Port, Errors) {
	p := &Port{Container: n, Published: n}
	if kind, ok := DatastorePort(n); ok {
		p.Skipped = true
		p.Reason = kind
	}
	return p, nil
}

func parseMount(svc string, node *yaml.Node) (*Mount, Errors) {
	bindFix := "Use x-simplecloud-sync for configuration, assets, or interpreted source, " +
		"or a named volume for data the service owns. A bind mount has no local filesystem to read here."
	switch node.Kind {
	case yaml.ScalarNode:
		spec := node.Value
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			return nil, Errors{&Error{Service: svc, Key: "volumes",
				Problem: fmt.Sprintf("anonymous volume %q is not supported", spec),
				Fix:     "Declare a named volume and mount it at a path."}}
		}
		source, target := parts[0], parts[1]
		if isBindSource(source) {
			return nil, Errors{&Error{Service: svc, Key: "volumes",
				Problem: fmt.Sprintf("bind mount %q is not supported", spec), Fix: bindFix}}
		}
		m := &Mount{Volume: source, Path: target}
		if len(parts) > 2 && parts[2] == "ro" {
			m.ReadOnly = true
		}
		return m, nil
	case yaml.MappingNode:
		var raw struct {
			Type     string `yaml:"type"`
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
		}
		if err := node.Decode(&raw); err != nil {
			return nil, Errors{&Error{Service: svc, Key: "volumes", Problem: err.Error()}}
		}
		if raw.Type == "bind" || isBindSource(raw.Source) {
			return nil, Errors{&Error{Service: svc, Key: "volumes",
				Problem: fmt.Sprintf("bind mount of %q is not supported", raw.Source), Fix: bindFix}}
		}
		if raw.Type != "" && raw.Type != "volume" {
			return nil, Errors{&Error{Service: svc, Key: "volumes",
				Problem: fmt.Sprintf("mount type %q is not supported", raw.Type),
				Fix:     "Only named volumes are supported."}}
		}
		return &Mount{Volume: raw.Source, Path: raw.Target, ReadOnly: raw.ReadOnly}, nil
	}
	return nil, Errors{&Error{Service: svc, Key: "volumes", Problem: "want a string or a mapping"}}
}

func isBindSource(source string) bool {
	return strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") ||
		strings.HasPrefix(source, "~") || strings.Contains(source, "${")
}

func parseHealthcheck(svc string, r *rawHealthcheck) (*Healthcheck, Errors) {
	var errs Errors
	hc := &Healthcheck{Retries: r.Retries, Disabled: r.Disable}
	if r.Test.Kind != 0 {
		test, terr := parseStringOrList(svc, "healthcheck.test", &r.Test)
		errs = append(errs, terr...)
		hc.Test = test
	}
	for _, f := range []struct {
		key, val string
		dst      *time.Duration
	}{{"interval", r.Interval, &hc.Interval}, {"timeout", r.Timeout, &hc.Timeout}} {
		if f.val == "" {
			continue
		}
		d, err := parseDuration(f.val)
		if err != nil {
			errs = append(errs, &Error{Service: svc, Key: "healthcheck." + f.key,
				Problem: fmt.Sprintf("%q is not a duration", f.val), Fix: "Use a value like 10s or 1m."})
			continue
		}
		*f.dst = d
	}
	return hc, errs
}

func parseMemory(v string) (int, error) {
	v = strings.TrimSpace(v)
	mult := 1
	switch {
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		mult, v = 1024, v[:len(v)-1]
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		mult, v = 1, v[:len(v)-1]
	case strings.HasSuffix(v, "GB"), strings.HasSuffix(v, "gb"):
		mult, v = 1024, v[:len(v)-2]
	case strings.HasSuffix(v, "MB"), strings.HasSuffix(v, "mb"):
		mult, v = 1, v[:len(v)-2]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("memory %q is not a size", v)
	}
	return int(n) * mult, nil
}

// parseExposed reads one expose: entry, which names a port reachable inside the
// project rather than one to publish.
func parseExposed(svc string, node *yaml.Node) (int, Errors) {
	spec := node.Value
	if base, proto, found := strings.Cut(spec, "/"); found {
		if strings.ToLower(proto) != "tcp" {
			return 0, Errors{&Error{Service: svc, Key: "expose",
				Problem: fmt.Sprintf("protocol %q is not supported", proto),
				Fix:     "Only TCP is carried between services."}}
		}
		spec = base
	}
	n, err := strconv.Atoi(strings.TrimSpace(spec))
	if err != nil || n < 1 || n > 65535 {
		return 0, Errors{&Error{Service: svc, Key: "expose",
			Problem: fmt.Sprintf("%q is not a valid port", node.Value)}}
	}
	return n, nil
}
