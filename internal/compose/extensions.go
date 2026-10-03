package compose

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Extensions are the x-simplecloud-* keys. Every one has a default, so a Compose
// file needs none of them.
const (
	extIdleTimeout  = "x-simplecloud-idle-timeout"
	extKeepAwake    = "x-simplecloud-keep-awake"
	extVCPU         = "x-simplecloud-vcpu"
	extMemory       = "x-simplecloud-memory"
	extRegion       = "x-simplecloud-region"
	extMaxTTL       = "x-simplecloud-max-ttl"
	extDoorbellPort = "x-simplecloud-doorbell-port"
	extLogRing      = "x-simplecloud-log-ring"
	extEnvironment  = "x-simplecloud-environment"
	extEnvFile      = "x-simplecloud-env-file"
	extEgress       = "x-simplecloud-egress"
	extSync         = "x-simplecloud-sync"
	extPublish      = "x-simplecloud-publish"
	extName         = "x-simplecloud-name"
)

var serviceExtensions = map[string]bool{
	extIdleTimeout: true, extKeepAwake: true, extVCPU: true, extMemory: true,
	extRegion: true, extMaxTTL: true, extDoorbellPort: true, extLogRing: true,
	extEnvironment: true, extEnvFile: true, extEgress: true, extSync: true,
	extPublish: true,
}

func knownExtension(key string) bool {
	if !strings.HasPrefix(key, "x-simplecloud-") {
		// Other x- keys belong to other tools and are none of our business.
		return true
	}
	return serviceExtensions[key]
}

func extensionNames() []string {
	out := make([]string, 0, len(serviceExtensions))
	for k := range serviceExtensions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parseExtensions(node *yaml.Node, s *Service) Errors {
	var errs Errors
	bad := func(key, problem, fix string) {
		errs = append(errs, &Error{Service: s.Name, Key: key, Problem: problem, Fix: fix})
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		if !strings.HasPrefix(key, "x-simplecloud-") {
			continue
		}
		switch key {
		case extIdleTimeout:
			d, err := parseDuration(val.Value)
			if err != nil {
				bad(key, fmt.Sprintf("%q is not a duration", val.Value), "Use a value like 15m, 90s, or 0 to never sleep.")
				continue
			}
			s.IdleTimeout = d
		case extKeepAwake:
			b, err := strconv.ParseBool(val.Value)
			if err != nil {
				bad(key, fmt.Sprintf("%q is not true or false", val.Value), "")
				continue
			}
			s.KeepAwake = b
			s.keepAwakeSet = true
		case extVCPU:
			n, err := strconv.Atoi(val.Value)
			if err != nil {
				bad(key, fmt.Sprintf("%q is not a number", val.Value), "")
				continue
			}
			s.VCPU = n
		case extMemory:
			if n, err := strconv.Atoi(val.Value); err == nil {
				s.MemMiB = n
				continue
			}
			mib, err := parseMemory(val.Value)
			if err != nil {
				bad(key, err.Error(), "Use MiB as a number, or a size like 512M or 2G.")
				continue
			}
			s.MemMiB = mib
		case extRegion:
			s.Region = val.Value
		case extMaxTTL:
			n, err := strconv.Atoi(val.Value)
			if err != nil {
				bad(key, fmt.Sprintf("%q is not a number of seconds", val.Value), "")
				continue
			}
			s.MaxTTL = n
		case extDoorbellPort:
			n, err := strconv.Atoi(val.Value)
			if err != nil || n < 1 || n > 65535 {
				bad(key, fmt.Sprintf("%q is not a valid port", val.Value), "")
				continue
			}
			s.DoorbellPort = n
		case extLogRing:
			size, err := parseByteSize(val.Value)
			if err != nil {
				bad(key, err.Error(), "Use a size like 4MiB. Keep it small: written bytes sit in page cache, and a pause snapshots RAM.")
				continue
			}
			s.LogRing = size
		case extEnvironment:
			env, eerr := parseEnvMap(s.Name, key, val)
			errs = append(errs, eerr...)
			s.DeployEnv = env
		case extEnvFile:
			s.DeployEnvFile = val.Value
		case extEgress:
			if val.Kind != yaml.SequenceNode {
				bad(key, "want a list of hosts", "")
				continue
			}
			for _, n := range val.Content {
				s.Egress = append(s.Egress, n.Value)
			}
		case extSync:
			if val.Kind != yaml.SequenceNode {
				bad(key, "want a list of local:path entries", "")
				continue
			}
			for _, n := range val.Content {
				local, target, found := strings.Cut(n.Value, ":")
				if !found || local == "" || target == "" {
					bad(key, fmt.Sprintf("%q is not local:path", n.Value), "Write it as ./config:/etc/app.")
					continue
				}
				if !strings.HasPrefix(target, "/") {
					bad(key, fmt.Sprintf("target %q is not absolute", target), "")
					continue
				}
				s.Sync = append(s.Sync, SyncPath{Local: local, Path: target})
			}
		case extPublish:
			if val.Kind != yaml.SequenceNode {
				bad(key, "want a list of ports", "")
				continue
			}
			for _, n := range val.Content {
				p, err := strconv.Atoi(n.Value)
				if err != nil || p < 1 || p > 65535 {
					bad(key, fmt.Sprintf("%q is not a valid port", n.Value), "")
					continue
				}
				s.PublishExtra = append(s.PublishExtra, p)
			}
		}
	}

	// An opted-in port stops being skipped. This is how a database is published
	// deliberately rather than by inheriting a local development convenience.
	for _, want := range s.PublishExtra {
		found := false
		for i := range s.Ports {
			if s.Ports[i].Container == want {
				s.Ports[i].Skipped = false
				s.Ports[i].Reason = ""
				found = true
			}
		}
		if !found {
			s.Ports = append(s.Ports, Port{Container: want, Published: want})
		}
	}
	return errs
}

func parseByteSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	var mult int64 = 1
	for _, suffix := range []struct {
		s string
		m int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, suffix.s) {
			mult = suffix.m
			v = strings.TrimSpace(strings.TrimSuffix(v, suffix.s))
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size", v)
	}
	return int64(n * float64(mult)), nil
}
