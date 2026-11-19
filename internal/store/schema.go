package store

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS instances (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		container_id TEXT NOT NULL DEFAULT '',
		selected_repo TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		last_activity_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS runs (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL UNIQUE,
		instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
		agent TEXT NOT NULL,
		prompt TEXT NOT NULL,
		response TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		session_id TEXT NOT NULL DEFAULT '',
		repo TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		finished_at TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS runs_instance ON runs(instance_id, seq)`,
	`CREATE TABLE IF NOT EXISTS connections (
		instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
		provider TEXT NOT NULL,
		status TEXT NOT NULL,
		method TEXT NOT NULL DEFAULT '',
		account TEXT NOT NULL DEFAULT '',
		secret TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY (instance_id, provider)
	)`,
	`CREATE TABLE IF NOT EXISTS oauth_states (
		state_hash TEXT PRIMARY KEY,
		instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
		verifier TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
}
