// Package compose loads the Docker Compose subset Simplecloud supports.
//
// Unsupported keys are rejected with a message naming what to do instead, never
// ignored: silently dropping a key the author relied on changes what their
// application is without telling them.
package compose

import "time"

// Project is a parsed, validated Compose file.
type Project struct {
	Name     string
	Dir      string
	File     string
	Services []*Service
	Volumes  map[string]Volume
}

// Service is one Compose service.
type Service struct {
	Name string

	Image string
	Build *Build

	Command     []string
	Environment map[string]string
	EnvFiles    []string

	// Ports the author asked to publish. Datastore ports are recorded here with
	// Skipped set rather than dropped, so output can explain the decision.
	Ports []Port

	Mounts      []Mount
	Healthcheck *Healthcheck
	DependsOn   []string
	Profiles    []string

	Replicas int
	VCPU     int
	MemMiB   int

	IdleTimeout  time.Duration
	KeepAwake    bool
	keepAwakeSet bool
	Region       string
	MaxTTL       int
	DoorbellPort int
	LogRing      int64

	DeployEnv     map[string]string
	DeployEnvFile string
	Egress        []string
	Sync          []SyncPath
	PublishExtra  []int
}

// KeepAwakeExplicit reports whether the author set keep-awake, as opposed to it
// being defaulted from the absence of a reachable port.
func (s *Service) KeepAwakeExplicit() bool { return s.keepAwakeSet }

type Build struct {
	Context    string
	Dockerfile string
	Args       map[string]string
	Target     string
}

type Port struct {
	Container int
	Published int
	// Skipped is set for a datastore or administrative port, which is not
	// published unless the author opts in with x-simplecloud-publish.
	Skipped bool
	Reason  string
}

type Volume struct {
	Name string
}

// Mount is a named volume mounted at a path. Bind mounts are rejected at parse
// time, so they never reach here.
type Mount struct {
	Volume   string
	Path     string
	ReadOnly bool
}

// SyncPath is a local directory copied into a disk on every deploy.
type SyncPath struct {
	Local string
	Path  string
}

type Healthcheck struct {
	Test     []string
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
	Disabled bool
}

// Defaults applied to every service unless overridden.
const (
	DefaultReplicas     = 1
	DefaultVCPU         = 1
	DefaultMemMiB       = 1024
	DefaultIdleTimeout  = 15 * time.Minute
	DefaultMaxTTL       = 86400
	DefaultDoorbellPort = 48080
	DefaultLogRing      = 4 << 20
)

// datastorePorts are not published by a bare ports: entry. A Compose file written
// for local development routinely maps a database port so a client on the laptop
// can reach it; publishing that verbatim would put the database on the internet
// with no authentication. The list is exhaustive and lives only here.
var datastorePorts = map[int]string{
	22: "SSH", 23: "Telnet", 25: "SMTP", 445: "SMB",
	1433: "SQL Server", 1521: "Oracle", 2181: "ZooKeeper",
	2375: "Docker", 2376: "Docker", 3306: "MySQL", 3389: "RDP",
	5432: "PostgreSQL", 5672: "AMQP", 5984: "CouchDB", 6379: "Redis",
	6443: "Kubernetes API", 7000: "Cassandra", 7001: "Cassandra",
	8086: "InfluxDB", 9042: "Cassandra", 9092: "Kafka",
	9200: "Elasticsearch", 9300: "Elasticsearch", 11211: "Memcached",
	15672: "RabbitMQ management", 26257: "CockroachDB",
	27017: "MongoDB", 27018: "MongoDB",
}

// DatastorePort returns the service name for a well-known datastore or
// administrative port.
func DatastorePort(port int) (string, bool) {
	n, ok := datastorePorts[port]
	return n, ok
}

// CloudProfile is active when deploying and inactive under `docker compose up`,
// which is what lets a service exist only when deployed.
const CloudProfile = "cloud"
