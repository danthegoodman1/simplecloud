package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, body string) *Project {
	t.Helper()
	p, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("unexpected error:\n%v", err)
	}
	return p
}

func loadErr(t *testing.T, body string) string {
	t.Helper()
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("want an error, got none")
	}
	return err.Error()
}

// A service needs nothing but an image, and every other value is defaulted.
func TestMinimalFileAndDefaults(t *testing.T) {
	p := load(t, `
services:
  web:
    image: nginx
`)
	if len(p.Services) != 1 {
		t.Fatalf("want 1 service, got %d", len(p.Services))
	}
	s := p.Services[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"image", s.Image, "nginx"},
		{"replicas", s.Replicas, DefaultReplicas},
		{"vcpu", s.VCPU, DefaultVCPU},
		{"memory", s.MemMiB, DefaultMemMiB},
		{"idle timeout", s.IdleTimeout, DefaultIdleTimeout},
		{"max ttl", s.MaxTTL, DefaultMaxTTL},
		{"doorbell port", s.DoorbellPort, DefaultDoorbellPort},
		{"log ring", s.LogRing, int64(DefaultLogRing)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	// No reachable port means activity cannot be observed, so sleeping it would
	// be a guess. Default to keep-awake instead.
	if !s.KeepAwake {
		t.Error("a service with no port should default to keep-awake")
	}
	if s.KeepAwakeExplicit() {
		t.Error("the default must not look like an explicit choice")
	}
}

func TestProjectNameFromDirectoryAndOverrides(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	os.WriteFile(file, []byte("services:\n  a:\n    image: x\n"), 0o644)
	p, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != filepath.Base(dir) {
		t.Errorf("want project name %q, got %q", filepath.Base(dir), p.Name)
	}
	p2 := load(t, "x-simplecloud-name: chosen\nservices:\n  a:\n    image: x\n")
	if p2.Name != "chosen" {
		t.Errorf("x-simplecloud-name should set the label, got %q", p2.Name)
	}
}

// Every rejected key must name a remedy. An error that only says "unsupported"
// leaves the author guessing, which is the failure mode this guards.
func TestUnsupportedKeysRejectedWithRemedy(t *testing.T) {
	for key, want := range map[string]string{
		"restart":        "supervises",
		"networks":       "reach each other by name",
		"configs":        "x-simplecloud-sync",
		"secrets":        "x-simplecloud-env-file",
		"extends":        "Inline",
		"privileged":     "full capabilities",
		"container_name": "cannot be renamed",
		"cap_add":        "full capabilities",
		"devices":        "Remove",
		"links":          "by name",
		"network_mode":   "managed for you",
		"user":           "USER in the image",
	} {
		body := "services:\n  a:\n    image: x\n    " + key + ": " + unsupportedValue(key) + "\n"
		msg := loadErr(t, body)
		if !strings.Contains(msg, key) {
			t.Errorf("%s: error does not name the key: %s", key, msg)
		}
		if !strings.Contains(msg, want) {
			t.Errorf("%s: error lacks a remedy mentioning %q: %s", key, want, msg)
		}
	}
}

func unsupportedValue(key string) string {
	switch key {
	case "networks", "configs", "secrets", "cap_add", "devices", "links":
		return "[foo]"
	case "privileged":
		return "true"
	default:
		return "foo"
	}
}

// A bind mount is the one thing that forces a decision, so the error has to name
// both paths forward rather than just refusing.
func TestBindMountsRejectedWithBothRemedies(t *testing.T) {
	for _, spec := range []string{"./src:/app", "/abs/path:/app", "~/data:/data", "${PWD}/x:/x"} {
		msg := loadErr(t, "services:\n  a:\n    image: x\n    volumes:\n      - \""+spec+"\"\n")
		if !strings.Contains(msg, "x-simplecloud-sync") || !strings.Contains(msg, "named volume") {
			t.Errorf("%s: error must offer sync and named volume: %s", spec, msg)
		}
	}
	// The long form is the same decision.
	msg := loadErr(t, `
services:
  a:
    image: x
    volumes:
      - type: bind
        source: ./src
        target: /app
`)
	if !strings.Contains(msg, "x-simplecloud-sync") {
		t.Errorf("long-form bind mount: %s", msg)
	}
}

// A datastore port is skipped rather than rejected: the same file has to deploy
// here and still map to the laptop under `docker compose up`.
func TestDatastorePortsSkippedNotRejected(t *testing.T) {
	p := load(t, `
volumes:
  data: {}
services:
  postgres:
    image: postgres:17
    ports:
      - "5432:5432"
    volumes:
      - data:/var/lib/postgresql/data
  web:
    image: nginx
    ports:
      - "8080:80"
`)
	pg := p.Service("postgres")
	if len(pg.Ports) != 1 || !pg.Ports[0].Skipped {
		t.Fatalf("5432 should be skipped, got %+v", pg.Ports)
	}
	if !strings.Contains(pg.Ports[0].Reason, "PostgreSQL") {
		t.Errorf("reason should name the service, got %q", pg.Ports[0].Reason)
	}
	web := p.Service("web")
	if len(web.Ports) != 1 || web.Ports[0].Skipped {
		t.Errorf("a web port should publish, got %+v", web.Ports)
	}
	if web.KeepAwake {
		t.Error("a service with a reachable port should not default to keep-awake")
	}
}

func TestPublishOptInUnskipsPort(t *testing.T) {
	p := load(t, `
services:
  postgres:
    image: postgres:17
    ports:
      - "5432:5432"
    x-simplecloud-publish: [5432]
`)
	ports := p.Service("postgres").Ports
	if len(ports) != 1 || ports[0].Skipped {
		t.Errorf("explicit publish should override the skip, got %+v", ports)
	}
}

func TestEveryDatastorePortIsCovered(t *testing.T) {
	for _, port := range []int{22, 23, 25, 445, 1433, 1521, 2181, 2375, 2376, 3306, 3389,
		5432, 5672, 5984, 6379, 6443, 7000, 7001, 8086, 9042, 9092, 9200, 9300,
		11211, 15672, 26257, 27017, 27018} {
		if _, ok := DatastorePort(port); !ok {
			t.Errorf("port %d should be treated as a datastore port", port)
		}
	}
	if _, ok := DatastorePort(8080); ok {
		t.Error("8080 is a web port and must publish")
	}
}

func TestUDPAndRangesRejected(t *testing.T) {
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    ports:\n      - \"53:53/udp\"\n"); !strings.Contains(msg, "TCP") {
		t.Errorf("UDP should be rejected mentioning TCP: %s", msg)
	}
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    ports:\n      - \"3000-3005\"\n"); !strings.Contains(msg, "range") {
		t.Errorf("port range should be rejected: %s", msg)
	}
}

// profiles is how a service exists only when deployed, so the cloud profile must
// be active here and a local-only profile must not be.
func TestProfilesDecideWhatDeploys(t *testing.T) {
	p := load(t, `
services:
  postgres:
    image: postgres:17
  backups:
    image: pgbackup
    profiles: [cloud]
  mailcatcher:
    image: mailhog
    profiles: [local]
`)
	got := map[string]bool{}
	for _, s := range p.ActiveServices(nil) {
		got[s.Name] = true
	}
	if !got["postgres"] || !got["backups"] {
		t.Errorf("profile-less and cloud-profile services should deploy, got %v", got)
	}
	if got["mailcatcher"] {
		t.Error("a local-only service must not deploy")
	}
	with := map[string]bool{}
	for _, s := range p.ActiveServices([]string{"local"}) {
		with[s.Name] = true
	}
	if !with["mailcatcher"] {
		t.Error("an explicitly activated profile should deploy")
	}
}

func TestSlotNameCollisionRejected(t *testing.T) {
	msg := loadErr(t, `
services:
  db:
    image: postgres
  db-1:
    image: nginx
`)
	if !strings.Contains(msg, "slot") {
		t.Errorf("want a slot collision error, got: %s", msg)
	}
}

func TestExtensionsParsed(t *testing.T) {
	p := load(t, `
services:
  api:
    image: myapi
    ports: ["8080:8080"]
    x-simplecloud-idle-timeout: 90s
    x-simplecloud-keep-awake: true
    x-simplecloud-vcpu: 4
    x-simplecloud-memory: 2G
    x-simplecloud-region: aws-us-west-2
    x-simplecloud-max-ttl: 3600
    x-simplecloud-doorbell-port: 49090
    x-simplecloud-log-ring: 8MiB
    x-simplecloud-environment:
      S3_ENDPOINT: https://s3.amazonaws.com
    x-simplecloud-env-file: .env.cloud
    x-simplecloud-egress:
      - s3.amazonaws.com
    x-simplecloud-sync:
      - ./config:/etc/app
`)
	s := p.Service("api")
	if s.IdleTimeout != 90*time.Second || !s.KeepAwake || !s.KeepAwakeExplicit() {
		t.Errorf("idle/keep-awake wrong: %v %v", s.IdleTimeout, s.KeepAwake)
	}
	if s.VCPU != 4 || s.MemMiB != 2048 {
		t.Errorf("resources wrong: %d vcpu %d MiB", s.VCPU, s.MemMiB)
	}
	if s.Region != "aws-us-west-2" || s.MaxTTL != 3600 || s.DoorbellPort != 49090 {
		t.Errorf("region/ttl/doorbell wrong: %s %d %d", s.Region, s.MaxTTL, s.DoorbellPort)
	}
	if s.LogRing != 8<<20 {
		t.Errorf("log ring wrong: %d", s.LogRing)
	}
	if s.DeployEnv["S3_ENDPOINT"] != "https://s3.amazonaws.com" || s.DeployEnvFile != ".env.cloud" {
		t.Errorf("deploy env wrong: %v %q", s.DeployEnv, s.DeployEnvFile)
	}
	if len(s.Egress) != 1 || len(s.Sync) != 1 || s.Sync[0].Path != "/etc/app" {
		t.Errorf("egress/sync wrong: %v %v", s.Egress, s.Sync)
	}
}

func TestUnknownExtensionRejectedButForeignOnesIgnored(t *testing.T) {
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    x-simplecloud-nonsense: 1\n"); !strings.Contains(msg, "unknown extension") {
		t.Errorf("want unknown extension error, got %s", msg)
	}
	// Another tool's extension is none of our business.
	load(t, "services:\n  a:\n    image: x\n    x-some-other-tool: anything\n")
}

func TestResourceReservationsAndRanges(t *testing.T) {
	p := load(t, `
services:
  a:
    image: x
    deploy:
      replicas: 2
      resources:
        reservations:
          cpus: "1.5"
          memory: 512M
`)
	s := p.Service("a")
	if s.Replicas != 2 {
		t.Errorf("replicas: %d", s.Replicas)
	}
	// Fractional vCPU rounds up; a sandbox cannot be given half a core.
	if s.VCPU != 2 {
		t.Errorf("1.5 cpus should round up to 2, got %d", s.VCPU)
	}
	if s.MemMiB != 512 {
		t.Errorf("memory: %d", s.MemMiB)
	}
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    x-simplecloud-vcpu: 64\n"); !strings.Contains(msg, "out of range") {
		t.Errorf("want range error: %s", msg)
	}
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    x-simplecloud-max-ttl: 999999\n"); !strings.Contains(msg, "out of range") {
		t.Errorf("want ttl range error: %s", msg)
	}
}

func TestCommandAndEnvironmentForms(t *testing.T) {
	p := load(t, `
services:
  a:
    image: x
    command: "sh -c 'echo hi there'"
    environment:
      A: "1"
  b:
    image: x
    command: ["sh", "-c", "echo hi"]
    environment:
      - B=2
      - C
`)
	a, b := p.Service("a"), p.Service("b")
	if len(a.Command) != 3 || a.Command[2] != "echo hi there" {
		t.Errorf("string command not split with quotes honored: %q", a.Command)
	}
	if len(b.Command) != 3 || b.Command[0] != "sh" {
		t.Errorf("list command: %q", b.Command)
	}
	if a.Environment["A"] != "1" || b.Environment["B"] != "2" {
		t.Errorf("environment: %v %v", a.Environment, b.Environment)
	}
	if _, ok := b.Environment["C"]; !ok {
		t.Error("a bare NAME should be present, taking its value from the environment")
	}
}

func TestUndeclaredVolumeAndUnknownDependencyRejected(t *testing.T) {
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\n"); !strings.Contains(msg, "not declared") {
		t.Errorf("want undeclared volume error: %s", msg)
	}
	if msg := loadErr(t, "services:\n  a:\n    image: x\n    depends_on: [ghost]\n"); !strings.Contains(msg, "ghost") {
		t.Errorf("want unknown dependency error: %s", msg)
	}
}

func TestServiceNeedsImageOrBuild(t *testing.T) {
	if msg := loadErr(t, "services:\n  a:\n    command: x\n"); !strings.Contains(msg, "image: or build:") {
		t.Errorf("want image-or-build error: %s", msg)
	}
	p := load(t, "services:\n  a:\n    build: .\n")
	if b := p.Service("a").Build; b == nil || b.Context != "." || b.Dockerfile != "Dockerfile" {
		t.Errorf("short-form build defaults wrong: %+v", b)
	}
}

func TestNonAmd64PlatformRejected(t *testing.T) {
	msg := loadErr(t, "services:\n  a:\n    build:\n      context: .\n      platform: linux/arm64\n")
	if !strings.Contains(msg, "linux/amd64") {
		t.Errorf("want amd64 error: %s", msg)
	}
}

// Walking up to the nearest Compose file is what lets commands run from a
// subdirectory, the way git does.
func TestFindFileWalksUp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  a:\n    image: x\n"), 0o644)
	deep := filepath.Join(dir, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	got, err := FindFile(deep)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "compose.yaml") {
		t.Errorf("got %q", got)
	}
	if _, err := FindFile(t.TempDir()); err == nil {
		t.Error("want an error when no Compose file exists")
	}
}

// All problems in a file should be reported in one run.
func TestMultipleErrorsReportedTogether(t *testing.T) {
	msg := loadErr(t, `
services:
  a:
    image: x
    restart: always
    networks: [foo]
    volumes:
      - ./src:/app
`)
	for _, want := range []string{"restart", "networks", "bind mount"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in combined errors: %s", want, msg)
		}
	}
}
