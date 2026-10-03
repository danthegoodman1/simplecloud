// Package state is the local SQLite store. It records what was done, while
// configuration records where the hub is: a hub endpoint can be edited by hand
// without touching deployment state.
package state

const schemaVersion = 1

// migrations run in order and are recorded, so a store is safe to reopen.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,

	// A project is identified by the directory holding its Compose file, not by
	// its name: two checkouts sharing a basename stay distinct, and renaming a
	// directory does not orphan a project.
	`CREATE TABLE IF NOT EXISTS projects (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL,
		dir             TEXT NOT NULL UNIQUE,
		overlay_cidr    TEXT NOT NULL,
		overlay_index   INTEGER NOT NULL UNIQUE,
		hub_listen_port INTEGER NOT NULL UNIQUE,
		hub_public_key  TEXT NOT NULL DEFAULT '',
		region          TEXT NOT NULL,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS services (
		project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		name         TEXT NOT NULL,
		image        TEXT NOT NULL DEFAULT '',
		image_digest TEXT NOT NULL DEFAULT '',
		image_id     TEXT NOT NULL DEFAULT '',
		build_hash   TEXT NOT NULL DEFAULT '',
		config_hash  TEXT NOT NULL DEFAULT '',
		replicas     INTEGER NOT NULL DEFAULT 1,
		spec         TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY (project_id, name)
	);`,

	// A slot owns its WireGuard key and overlay address for life. Sandboxes are
	// disposable incarnations, so replacing one never rewrites hub configuration.
	`CREATE TABLE IF NOT EXISTS slots (
		project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		service           TEXT NOT NULL,
		ordinal           INTEGER NOT NULL,
		name              TEXT NOT NULL,
		overlay_ip        TEXT NOT NULL,
		loopback_ip       TEXT NOT NULL DEFAULT '',
		wg_private_key    TEXT NOT NULL,
		wg_public_key     TEXT NOT NULL,
		sandbox_id        TEXT NOT NULL DEFAULT '',
		sandbox_status    TEXT NOT NULL DEFAULT '',
		doorbell_port     INTEGER NOT NULL DEFAULT 48080,
		doorbell_host     TEXT NOT NULL DEFAULT '',
		doorbell_token    TEXT NOT NULL DEFAULT '',
		doorbell_token_id TEXT NOT NULL DEFAULT '',
		doorbell_expires  INTEGER NOT NULL DEFAULT 0,
		agent_process_id  TEXT NOT NULL DEFAULT '',
		log_cursor        INTEGER NOT NULL DEFAULT 0,
		last_active_at    INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (project_id, name)
	);`,

	`CREATE TABLE IF NOT EXISTS volumes (
		project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		name        TEXT NOT NULL,
		disk_id     TEXT NOT NULL DEFAULT '',
		mount_token TEXT NOT NULL DEFAULT '',
		owner_slot  TEXT NOT NULL DEFAULT '',
		mount_path  TEXT NOT NULL DEFAULT '',
		kind        TEXT NOT NULL DEFAULT 'volume',
		PRIMARY KEY (project_id, name)
	);`,

	`CREATE TABLE IF NOT EXISTS exposures (
		project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		slot       TEXT NOT NULL,
		port       INTEGER NOT NULL,
		hostname   TEXT NOT NULL,
		PRIMARY KEY (project_id, slot, port)
	);`,

	// The pinned host key is state, not configuration: it records what was
	// observed, and a later mismatch is a reason to stop rather than a setting.
	`CREATE TABLE IF NOT EXISTS hub (
		id           INTEGER PRIMARY KEY CHECK (id = 1),
		ssh_target   TEXT NOT NULL,
		endpoint     TEXT NOT NULL,
		host_key     TEXT NOT NULL DEFAULT '',
		bootstrapped INTEGER NOT NULL DEFAULT 0,
		updated_at   INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS registry (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		host       TEXT NOT NULL,
		username   TEXT NOT NULL DEFAULT '',
		password   TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL
	);`,
}
