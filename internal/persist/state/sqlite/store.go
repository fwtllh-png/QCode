// Package sqlite owns QCode's durable relational state.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const SchemaVersion = 4

// schemaMigration moves the schema exactly one version forward. The step
// owns its transaction and records the new user_version itself.
type schemaMigration struct {
	from int
	to   int
	run  func(context.Context, *Store) error
}

// schemaMigrations is the explicit migration chain. Adding a schema version
// means appending one entry whose from equals the previous entry's to (or 3
// for the first entry); databases at a version with no registered step fail
// closed instead of being guessed.
var schemaMigrations = []schemaMigration{
	{
		from: 3,
		to:   4,
		run:  func(ctx context.Context, s *Store) error { return s.migrateV3ToV4(ctx) },
	},
}

var (
	ErrCorrupt           = errors.New("sqlite database is corrupt")
	ErrUnsupportedSchema = errors.New("sqlite schema version is unsupported")
)

func IsUniqueConstraintViolation(err error) bool {
	code, ok := errorCode(err)
	return ok && (code == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
		code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

func isCorruption(err error) bool {
	code, ok := errorCode(err)
	if !ok {
		return false
	}
	switch code & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	default:
		return false
	}
}

func errorCode(err error) (int, bool) {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code(), true
}

// CorruptionError reports database content that SQLite cannot safely read.
type CorruptionError struct {
	Path   string
	Detail string
	Err    error
}

func (e *CorruptionError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("sqlite database %q is corrupt: %s", e.Path, e.Detail)
	}
	return fmt.Sprintf("sqlite database %q is corrupt: %v", e.Path, e.Err)
}

func (e *CorruptionError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrCorrupt}
	}
	return []error{ErrCorrupt, e.Err}
}

// SchemaVersionError is returned without migrating or otherwise writing the
// database when its schema does not match the version this binary requires.
type SchemaVersionError struct {
	Found     int
	Supported int
}

func (e *SchemaVersionError) Error() string {
	if e.Found < e.Supported {
		return fmt.Sprintf(
			"sqlite schema version %d is older than required version %d; automatic migration is not supported",
			e.Found,
			e.Supported,
		)
	}
	return fmt.Sprintf("sqlite schema version %d is newer than supported version %d", e.Found, e.Supported)
}

func (e *SchemaVersionError) Unwrap() error { return ErrUnsupportedSchema }

// Options controls connection-local SQLite behavior.
type Options struct {
	// BusyTimeout is the maximum time SQLite waits for a locked database.
	// Zero selects the default of five seconds.
	BusyTimeout time.Duration
}

// Store is a concurrency-safe handle to the QCode state database.
type Store struct {
	path      string
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

// Open opens path, creates the current schema when needed, and verifies database
// integrity. A newer schema is rejected before any write is attempted.
func Open(ctx context.Context, path string, options ...Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	if len(options) > 1 {
		return nil, errors.New("sqlite Open accepts at most one Options value")
	}
	opts := Options{}
	if len(options) == 1 {
		opts = options[0]
	}
	if opts.BusyTimeout < 0 {
		return nil, errors.New("sqlite busy timeout cannot be negative")
	}
	if opts.BusyTimeout == 0 {
		opts.BusyTimeout = 5 * time.Second
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	dsn := sqliteDSN(absolute, opts.BusyTimeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// Keeping one live connection ensures every operation uses the connection
	// configured by the DSN pragmas. Separate Store handles still exercise
	// SQLite's normal cross-connection locking behavior.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{path: absolute, db: db}
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, store.classify("open database", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, store.classify("read schema version", err)
	}
	if version > SchemaVersion {
		return nil, &SchemaVersionError{Found: version, Supported: SchemaVersion}
	}

	if err := store.enableWAL(ctx); err != nil {
		return nil, err
	}
	if version == 0 {
		if err := store.initializeSchema(ctx); err != nil {
			return nil, err
		}
	} else if version != SchemaVersion {
		migrated, err := store.applySchemaMigrations(ctx, version)
		if err != nil {
			return nil, err
		}
		if migrated != SchemaVersion {
			return nil, &SchemaVersionError{
				Found: migrated, Supported: SchemaVersion,
			}
		}
	}
	if err := store.verifyPragmas(ctx, opts.BusyTimeout); err != nil {
		return nil, err
	}
	if err := store.verifyIntegrity(ctx); err != nil {
		return nil, err
	}

	ok = true
	return store, nil
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	u.RawQuery = query.Encode()
	return u.String()
}

func (s *Store) enableWAL(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return s.classify("enable WAL", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("enable WAL: SQLite selected journal mode %q", mode)
	}
	return nil
}

// applySchemaMigrations walks the explicit migration chain forward from
// version until it reaches SchemaVersion. A version without a registered
// step reports the unsupported-schema error without writing anything.
func (s *Store) applySchemaMigrations(ctx context.Context, version int) (int, error) {
	for version < SchemaVersion {
		step, ok := schemaMigrationFrom(version)
		if !ok {
			return version, &SchemaVersionError{
				Found: version, Supported: SchemaVersion,
			}
		}
		if err := step.run(ctx, s); err != nil {
			return version, err
		}
		version = step.to
	}
	return version, nil
}

func schemaMigrationFrom(from int) (schemaMigration, bool) {
	for _, step := range schemaMigrations {
		if step.from == from {
			return step, true
		}
	}
	return schemaMigration{}, false
}

func (s *Store) initializeSchema(ctx context.Context) error {
	return s.WithTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schemaCurrent); err != nil {
			return s.classify("create schema v1", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
			return s.classify("record schema version", err)
		}
		return nil
	})
}

func (s *Store) migrateV3ToV4(ctx context.Context) error {
	return s.WithTx(ctx, nil, func(tx *sql.Tx) error {
		for _, statement := range []string{
			`ALTER TABLE usage ADD COLUMN model_metadata_json
			 TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(model_metadata_json))`,
			`ALTER TABLE usage_turn_context ADD COLUMN model_metadata_json
			 TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(model_metadata_json))`,
			`PRAGMA user_version = 4`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return s.classify("migrate schema v3 to v4", err)
			}
		}
		return nil
	})
}

func (s *Store) verifyPragmas(ctx context.Context, timeout time.Duration) error {
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return s.classify("verify foreign keys", err)
	}
	if foreignKeys != 1 {
		return errors.New("sqlite foreign key enforcement is disabled")
	}
	var busyTimeout int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return s.classify("verify busy timeout", err)
	}
	if busyTimeout != timeout.Milliseconds() {
		return fmt.Errorf("sqlite busy timeout is %dms, want %dms", busyTimeout, timeout.Milliseconds())
	}
	return nil
}

func (s *Store) verifyIntegrity(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return s.classify("run integrity check", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return s.classify("read integrity check", err)
		}
		if result != "ok" {
			return &CorruptionError{Path: s.path, Detail: result}
		}
	}
	if err := rows.Err(); err != nil {
		return s.classify("run integrity check", err)
	}
	return nil
}

func (s *Store) classify(operation string, err error) error {
	if err == nil {
		return nil
	}
	if isCorruption(err) {
		return &CorruptionError{Path: s.path, Err: err}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// DB exposes the underlying handle for typed queries and prepared statements.
// Callers should prefer Transaction or WithTx for multi-statement writes.
func (s *Store) DB() *sql.DB { return s.db }

// BeginTx starts a transaction with the requested database/sql options.
func (s *Store) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, options)
	if err != nil {
		return nil, s.classify("begin transaction", err)
	}
	return tx, nil
}

// Transaction executes fn atomically with default transaction options.
func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.WithTx(ctx, nil, fn)
}

// WithTx executes fn atomically. It commits only when fn returns nil.
func (s *Store) WithTx(ctx context.Context, options *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	if fn == nil {
		return errors.New("sqlite transaction callback is required")
	}
	return s.classify("transaction", sqlkit.WithTx(ctx, s.db, options, fn))
}

// Close closes the database. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

const schemaCurrent = `
CREATE TABLE workspaces (
    id TEXT PRIMARY KEY,
    root_path TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (json_valid(metadata_json))
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    closed_at TEXT,
    CHECK (json_valid(metadata_json))
);
CREATE INDEX sessions_workspace_created ON sessions(workspace_id, created_at);

CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL,
    source_cursor INTEGER NOT NULL DEFAULT 0,
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX threads_session_created ON threads(session_id, created_at);

CREATE TABLE operations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    idempotency_key TEXT,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    request_json TEXT NOT NULL,
    response_json TEXT,
    error_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (json_valid(request_json)),
    CHECK (response_json IS NULL OR json_valid(response_json)),
    CHECK (error_json IS NULL OR json_valid(error_json)),
    UNIQUE (session_id, idempotency_key)
);
CREATE INDEX operations_session_created ON operations(session_id, created_at);

CREATE TABLE turns (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    operation_id TEXT REFERENCES operations(id) ON DELETE SET NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT,
    UNIQUE (thread_id, ordinal)
);
CREATE INDEX turns_thread_created ON turns(thread_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS turns_one_active_per_thread
ON turns(thread_id) WHERE status = 'active';

CREATE TABLE turn_domain_facts (
    turn_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    fact_json TEXT NOT NULL CHECK (json_valid(fact_json)),
    PRIMARY KEY (turn_id, sequence)
);

CREATE TABLE turn_terminal_envelopes (
    turn_id TEXT PRIMARY KEY,
    effect_id TEXT NOT NULL,
    digest TEXT NOT NULL,
    envelope_json TEXT NOT NULL CHECK (json_valid(envelope_json)),
    marker_json TEXT NOT NULL CHECK (json_valid(marker_json))
);

CREATE TABLE turn_terminal_outbox (
    turn_id TEXT NOT NULL,
    entry_id TEXT NOT NULL,
    published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0, 1)),
    PRIMARY KEY (turn_id, entry_id)
);

CREATE TABLE turn_coordinator_leases (
    turn_id TEXT PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX turn_coordinator_leases_expiry
ON turn_coordinator_leases(expires_at);

CREATE TABLE items (
    id TEXT PRIMARY KEY,
    turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    kind TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (json_valid(payload_json)),
    UNIQUE (turn_id, ordinal)
);

CREATE TABLE event_reservations (
    sequence INTEGER PRIMARY KEY CHECK (sequence > 0),
    event_id TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('reserved', 'committed', 'abandoned')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX event_reservations_status_sequence ON event_reservations(status, sequence);

CREATE TABLE event_index (
    sequence INTEGER PRIMARY KEY CHECK (sequence > 0),
    event_id TEXT NOT NULL UNIQUE,
    thread_id TEXT,
    turn_id TEXT,
    item_id TEXT,
    kind TEXT NOT NULL,
    log_offset INTEGER NOT NULL CHECK (log_offset >= 0),
    log_length INTEGER NOT NULL CHECK (log_length > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
    created_at TEXT NOT NULL
);
CREATE INDEX event_index_thread_sequence ON event_index(thread_id, sequence);
CREATE INDEX event_index_turn_sequence ON event_index(turn_id, sequence);

CREATE TABLE snapshots (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_id TEXT REFERENCES turns(id) ON DELETE SET NULL,
    cursor INTEGER NOT NULL CHECK (cursor >= 0),
    kind TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    CHECK (json_valid(metadata_json)),
    UNIQUE (thread_id, cursor, kind)
);
CREATE INDEX snapshots_thread_cursor ON snapshots(thread_id, cursor DESC);

CREATE TABLE usage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL,
    turn_id TEXT REFERENCES turns(id) ON DELETE SET NULL,
    sample INTEGER NOT NULL DEFAULT 0,
    event_sequence INTEGER,
    source_sequence INTEGER NOT NULL DEFAULT 0,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    model_metadata_json TEXT NOT NULL DEFAULT '{}',
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
    cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
    cost_microunits INTEGER NOT NULL DEFAULT 0 CHECK (cost_microunits >= 0),
    cost_known INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    CHECK (json_valid(model_metadata_json)),
    UNIQUE (turn_id, sample)
);
CREATE INDEX usage_session_created ON usage(session_id, created_at);
CREATE INDEX usage_turn ON usage(turn_id);

` + usageContextSchema +
	agentTopologySchema + repositoryIndexSchema +
	traceSchema + providerCapabilitySchema + contextRebaseSchema + `
`

const contextRebaseSchema = `
CREATE TABLE context_rebases (
    compaction_id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    envelope_digest TEXT NOT NULL,
    manifest_json TEXT NOT NULL CHECK (json_valid(manifest_json)),
    created_at TEXT NOT NULL,
    UNIQUE (thread_id, revision)
);
CREATE INDEX context_rebases_thread_revision
ON context_rebases(thread_id, revision DESC);

CREATE TABLE context_current (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    compaction_id TEXT NOT NULL REFERENCES context_rebases(compaction_id) ON DELETE CASCADE,
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    revision INTEGER NOT NULL CHECK (revision > 0),
    updated_at TEXT NOT NULL
);
`

const usageContextSchema = `
CREATE TABLE IF NOT EXISTS usage_turn_context (
    turn_id TEXT PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    model_metadata_json TEXT NOT NULL DEFAULT '{}',
    source_sequence INTEGER NOT NULL CHECK (source_sequence > 0),
    updated_at TEXT NOT NULL,
    CHECK (json_valid(model_metadata_json))
);
`

// Repository index rows are keyed by the canonical workspace root rather than a
// workspaces(id) reference: sessions that run without a persistent store keep
// their index in an ephemeral database that has no workspace rows at all, and
// one database may hold several roots.
const repositoryIndexSchema = `
CREATE TABLE repo_index_files (
    root_path TEXT NOT NULL,
    path TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    modified_unix_nano INTEGER NOT NULL DEFAULT 0,
    digest TEXT NOT NULL,
    symbol_count INTEGER NOT NULL DEFAULT 0,
    indexed_at TEXT NOT NULL,
    PRIMARY KEY (root_path, path)
);

CREATE TABLE repo_index_symbols (
    root_path TEXT NOT NULL,
    path TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    container TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL,
    exported INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (root_path, path)
        REFERENCES repo_index_files(root_path, path) ON DELETE CASCADE
);
CREATE INDEX repo_index_symbols_name ON repo_index_symbols(root_path, name);
CREATE INDEX repo_index_symbols_path ON repo_index_symbols(root_path, path);

CREATE TABLE repo_index_meta (
    root_path TEXT PRIMARY KEY,
    indexer_version INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    file_count INTEGER NOT NULL DEFAULT 0,
    symbol_count INTEGER NOT NULL DEFAULT 0,
    truncated INTEGER NOT NULL DEFAULT 0,
    refreshed_at TEXT NOT NULL
);
`

// A local trace has one row per span, keyed by the turn it belongs to.
//
// A trace is exactly one turn, so turn_id is the correlation key and there is no
// separate trace id: a column that only ever repeated turn_id would be one more
// thing to keep in agreement with it. Run-level traces spanning several turns
// would require a parent id here. Until a consumer needs that relationship,
// omitting it is the honest state.
//
// span_id is a per-turn counter rather than a random identifier so a trace reads
// in the order it happened and a test can name a span.
//
// Deleting the turn deletes its spans, which is how a deleted session takes its
// traces with it: turns cascade from threads, and threads from sessions. Nothing
// else expires them; span retention follows the wider event-retention policy.
const traceSchema = `
CREATE TABLE spans (
    turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    span_id INTEGER NOT NULL CHECK (span_id > 0),
    -- parent_span_id is null for the turn's root span.
    parent_span_id INTEGER,
    name TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
    -- status is ok / error / canceled, or open for a span whose own code path
    -- never closed it.
    status TEXT NOT NULL,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (turn_id, span_id),
    CHECK (json_valid(attributes_json))
);
CREATE INDEX spans_turn_started ON spans(turn_id, started_at);
CREATE INDEX spans_name ON spans(name);
`

const providerCapabilitySchema = `
CREATE TABLE provider_capabilities (
    provider_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    capability TEXT NOT NULL,
    supported INTEGER NOT NULL CHECK (supported IN (0, 1)),
    source TEXT NOT NULL,
    detail TEXT,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (provider_id, model_id, capability)
);
CREATE INDEX provider_capabilities_model ON provider_capabilities(provider_id, model_id);
`
