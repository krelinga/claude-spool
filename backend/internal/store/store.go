// Package store holds Spool's durable state: job records, per-queue runtime
// flags, and the executor's shared blocking state.
//
// Queue *definitions* deliberately do not live here — they come from
// queues.yaml (design §3.3). The database holds runtime state and history only.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database and applies migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One connection. Spool is a single-user service running one job at a time;
	// serialising database access costs nothing at this scale and removes a
	// whole class of SQLITE_BUSY races.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and health checks.
func (s *Store) DB() *sql.DB { return s.db }

// migrations are append-only: never edit one that has shipped.
var migrations = []string{
	`
CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  queue TEXT NOT NULL,
  queue_config_hash TEXT NOT NULL,
  status TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  input TEXT, args TEXT,
  rendered_prompt TEXT,
  model TEXT, labels TEXT, client_ref TEXT,
  submitted_by TEXT NOT NULL,
  idempotency_key TEXT,
  parent_job_id TEXT REFERENCES jobs(id),
  resume_session TEXT,
  run_attempts INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER,
  error_kind TEXT, error_message TEXT,
  outcome TEXT, summary TEXT,
  session_id TEXT, num_turns INTEGER, cost_usd REAL, duration_ms INTEGER,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  permission_denials TEXT,
  UNIQUE (queue, idempotency_key)
);
CREATE INDEX jobs_sched ON jobs(queue, status, priority DESC, id);
CREATE INDEX jobs_created ON jobs(created_at DESC);

CREATE TABLE queue_runtime (
  queue TEXT PRIMARY KEY,
  paused INTEGER NOT NULL DEFAULT 0,
  paused_reason TEXT,
  rr_credit REAL NOT NULL DEFAULT 0
);

CREATE TABLE executor_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  state TEXT NOT NULL,
  blocked_until INTEGER, reason TEXT
);

CREATE TABLE auth_events (
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT
);

CREATE TABLE webhook_outbox (
  id INTEGER PRIMARY KEY, event TEXT NOT NULL, queue TEXT, job_id TEXT,
  payload TEXT NOT NULL, url TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL, state TEXT NOT NULL
);

INSERT INTO executor_state (id, state) VALUES (1, 'ready');
`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Timestamps are stored as milliseconds since the Unix epoch.
func toMillis(t time.Time) int64    { return t.UnixMilli() }
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
func millisPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMillis(v.Int64)
	return &t
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
