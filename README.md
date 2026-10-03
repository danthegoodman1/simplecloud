# simplecloud

Deploy a Docker Compose project so that each service runs in its own microVM,
services reach each other by name over a private network, and idle services sleep
and wake on demand.

```
export ARCHIL_API_KEY=...
export SIMPLECLOUD_HUB=root@203.0.113.10
simplecloud hub add --bootstrap
simplecloud up
```

Compute and storage come from [Archil](https://archil.com). The private network is
WireGuard, with a hub you provide. State lives in local SQLite; there is no
central service to run.

## What you need

**An Archil API key**, on a plan that permits egress. A sandbox with no egress
cannot reach the hub, so no overlay can form. `simplecloud doctor` reports this
rather than letting it present as everything timing out.

**A Linux host for the WireGuard hub** with a public address, SSH access, and
inbound UDP `51820–51899`. One core and 512 MiB is enough; choose for bandwidth,
not CPU, because every byte between services passes through it. Any current Ubuntu
works and `--bootstrap` prepares a host with nothing installed.

**Docker with buildx**, only if a service uses `build:`.

Three things stay yours, because the CLI cannot do them: providing the host,
opening the UDP range if a firewall sits in front of it, and DNS.

## Setting up the hub

```
simplecloud hub add root@203.0.113.10 --identity ~/.ssh/id_ed25519 --bootstrap
```

This pins the host's SSH key, installs `wireguard-tools` and `nftables`, enables
IPv4 forwarding, writes the firewall, and secures `/etc/wireguard`. It is
re-runnable, and `simplecloud hub status` shows interfaces, peers, and handshakes.

A peer that never completes a handshake is the symptom of a blocked UDP port.

## Configuration

Environment variables are enough; no file is required.

| Variable | Meaning |
| --- | --- |
| `ARCHIL_API_KEY` | Compute credential |
| `SIMPLECLOUD_HUB` | Hub SSH target, such as `root@203.0.113.10` |
| `SIMPLECLOUD_HUB_IDENTITY` | SSH identity. Defaults to the agent, then the usual key names |
| `SIMPLECLOUD_HUB_ENDPOINT` | WireGuard endpoint, when the data path differs from the SSH path |
| `SIMPLECLOUD_REGION` | Compute region, default `aws-us-east-1` |
| `SIMPLECLOUD_REGISTRY` | Registry for built images |
| `SIMPLECLOUD_PROFILES` | Extra Compose profiles to activate |
| `SIMPLECLOUD_HOME` | Overrides `~/.simplecloud` |

Precedence is flag, then environment, then `~/.simplecloud/config.toml`.
`hub add` writes what you pass into that file, so later runs need no environment.

A project's identity is bound to the directory holding its Compose file, so two
checkouts sharing a name stay distinct and renaming a directory does not orphan a
project. If you move one, `simplecloud link <project>` rebinds it. Nothing is ever
written inside your project tree.

## Commands

| | |
| --- | --- |
| `up [service…]` | Converge. Named services update only those |
| `sleep` / `wake [service…]` | Pause and resume explicitly |
| `reap` | Pause everything past its idle timeout. The cron target |
| `down` | Destroy the project. `--volumes` also deletes disks |
| `ls` `ps` `show` | Projects, slots, and one slot in detail |
| `endpoints` `url` | Public URLs and in-project addresses |
| `logs` `exec` | Output and a shell |
| `volumes` | Disks, slots, and delegation holders |
| `hub add` / `hub status` / `hub set-endpoint` | Register and inspect the hub |
| `link` `plan` `reconcile` `doctor` | Rebind, dry run, repair drift, preflight |

`down` destroys and `sleep` pauses. They are separate verbs because sleeping is
the product's central behavior and conflating them would be a trap.

## The Compose subset

Supported: `image`, `build`, `command`, `environment`, `env_file`, `ports`,
`volumes` (named), `healthcheck`, `depends_on`, `profiles`, `deploy.replicas`, and
`deploy.resources.reservations`. Everything else is rejected with a message naming
what to do instead, rather than ignored.

A service needs only `image:` or `build:`. Everything else has a default, and most
defaults come from the image itself: `command` from its entrypoint and cmd,
environment from its env, reachable ports from its exposed ports. So this deploys:

```yaml
services:
  web:
    image: nginx
  postgres:
    image: postgres:17
```

### Extensions

| Key | Default | Meaning |
| --- | --- | --- |
| `x-simplecloud-idle-timeout` | `15m` | Idle duration before sleeping. `0` never sleeps |
| `x-simplecloud-keep-awake` | true with no reachable port | Never sleep |
| `x-simplecloud-vcpu` | `deploy.resources`, else 1 | 1–32 |
| `x-simplecloud-memory` | `deploy.resources`, else 1024 | MiB, 256–65536 |
| `x-simplecloud-region` | operator config | Compute region |
| `x-simplecloud-max-ttl` | `86400` | Hard TTL, capped at 24h |
| `x-simplecloud-doorbell-port` | `48080` | Agent listener port |
| `x-simplecloud-log-ring` | `4MiB` | In-sandbox log window |
| `x-simplecloud-environment` | none | Variables applied only when deployed |
| `x-simplecloud-env-file` | none | Env file read only when deployed |
| `x-simplecloud-egress` | unrestricted | Outbound allowlist |
| `x-simplecloud-sync` | none | Local paths copied into a disk each deploy |
| `x-simplecloud-publish` | none | Publish a port that is skipped by default |
| `x-simplecloud-name` | directory name | Project label, at the top level |

## Going from local to deployed

The same file serves both, and **bind mounts are the only thing that forces a
change**.

**Datastore ports are skipped, not rejected.** A Compose file written for local
development routinely maps a database port so a client on your laptop can reach
it. Locally that keeps working. Deploying it verbatim would put the database on the
internet with no authentication, so the entry is skipped and the deploy continues:

```
postgres  not publishing 5432 — datastore port, and published ports are unauthenticated
          reachable in this project as postgres:5432
          to publish anyway: x-simplecloud-publish: [5432]
```

**A service can exist only when deployed.** `profiles: [cloud]` is active here and
inactive under `docker compose up`, so a backup service deploys without running
locally. `profiles: [local]` is the reverse.

**Deploy-only configuration** replaces a local endpoint without a second file:

```yaml
services:
  minio:
    image: minio/minio
    profiles: [local]
  api:
    image: myapi
    environment:
      S3_ENDPOINT: http://minio:9000
    x-simplecloud-environment:
      S3_ENDPOINT: https://s3.us-east-1.amazonaws.com
    x-simplecloud-env-file: .env.cloud
```

**Bind mounts** need a decision, because only you know whether a path holds
configuration, data, or source. Use `x-simplecloud-sync` for configuration and
assets, which copies local into a disk on every deploy — one-way and clobbering, so
never point it at a path a service writes to. Use a **named volume** for data the
service owns.

## How sleeping works

Activity is an established inbound connection on a declared port. A public service
sees those arrive from the internet; a private one sees them over the overlay. One
rule covers both.

Outbound connections are not activity for the service that opens them, or an API
holding an idle pool would look busy forever. A service with no reachable port
cannot be observed at all, so it defaults to keep-awake.

**Sleeping cascades.** With no traffic, an API sleeps; its agent drains its
outbound connections first, so its database sees a clean FIN, goes idle, and sleeps
in turn. The total is the sum of the timeouts along the chain, not the longest.

**Waking is automatic.** A request to a public URL resumes that service. When it
then connects to a private service by name, the local relay rings that service's
doorbell — an authenticated request that resumes it, where an unauthenticated one
is refused and leaves it asleep. So a database needs no public port to be reachable
on demand.

Run `reap` from cron for automatic sleeping. It also pulls new log data on the same
round trip, so your cron cadence becomes your log capture cadence.

```
*/1 * * * *  simplecloud reap --all
```

## Logs

The sandbox keeps a small window — 4 MiB by default — because bytes written there
land in page cache and pausing snapshots RAM, so a large cache would slow every
sleep. History accumulates on your machine, pulled whenever a command talks to a
slot.

`logs` prints the last 200 lines. `-f` prints the backlog first, then streams.
Reading a sleeping slot serves stored output rather than waking it; `--wake` or
`-f` resumes it deliberately.

## Known limits

- **All traffic passes through the hub.** No sandbox has an inbound UDP endpoint,
  so there is no spoke-to-spoke path. The hub is a single point of failure and the
  bandwidth ceiling.
- **Everything pauses within 24 hours.** The platform caps a sandbox's lifetime, so
  "running" is never durable. Wake-on-demand covers it.
- **Published ports are public and unauthenticated**, any TCP over TLS with SNI.
  There are no custom domains.
- **amd64 only.**
- **No per-connection replica balancing, no rolling updates, no UDP between
  services.** The base service name resolves to slot 1.
- A **scheduled job** has no reachable port, so it defaults to keep-awake and bills
  continuously. Until scheduled wake exists, a cron beside `reap` is cheaper:
  `simplecloud exec postgres -- pg_dump …`.
- **State is per-machine.** Overlay ranges and hub listen ports are allocated from
  the local database, so two machines deploying to one hub would allocate the same
  values and collide. One operator per hub, for now.

## Development

```
./scripts/build.sh      # cross-compiles the agent for linux/amd64, embeds it, builds the CLI
go test ./...           # unit tests, no network
```

Integration tests need a live account and hub:

```
export ARCHIL_API_KEY=... SC_TEST_HUB=root@... SC_TEST_HUB_IDENTITY=~/.ssh/id_ed25519
go test -tags integration ./...
```

They create uniquely named resources and delete them on cleanup, including after a
failure, and fail the run if anything is left behind.
