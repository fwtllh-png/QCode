package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesSchemaAndConfiguresPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(t.Context(), path, Options{BusyTimeout: 137 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	assertPragma(t, store.DB(), "user_version", "4")
	assertPragma(t, store.DB(), "journal_mode", "wal")
	assertPragma(t, store.DB(), "foreign_keys", "1")
	assertPragma(t, store.DB(), "busy_timeout", "137")

	wantTables := []string{
		"workspaces", "sessions", "threads", "turns", "items", "operations",
		"event_reservations", "event_index",
		"turn_domain_facts", "turn_terminal_envelopes",
		"turn_terminal_outbox", "turn_coordinator_leases",
		"snapshots", "usage", "usage_turn_context",
		"agent_nodes", "agent_messages", "agent_results", "agent_budget_ledger",
		"agent_integrations",
		"repo_index_files", "repo_index_symbols", "repo_index_meta",
		"spans",
		"provider_capabilities",
		"context_rebases", "context_current",
	}
	for _, table := range wantTables {
		var count int
		err := store.DB().QueryRowContext(
			t.Context(),
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
			table,
		).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("table %q count = %d, want 1", table, count)
		}
	}

	assertTableColumns(t, store.DB(), "threads", "source_cursor")
	assertTableColumns(t, store.DB(), "usage",
		"sample", "source_sequence", "cost_known", "model_metadata_json")
	assertTableColumns(
		t,
		store.DB(),
		"usage_turn_context",
		"model_metadata_json",
	)
	assertTableColumns(t, store.DB(), "agent_nodes",
		"workspace_root", "session_id", "path", "execution_root", "revision",
		"owned_paths_json",
		"max_steps", "max_tokens", "max_cost_microunits",
		"reserved_tokens", "reserved_microunits", "operation_id")
	assertTableColumns(t, store.DB(), "agent_integrations",
		"workspace_root", "agent_id", "preview_digest", "status",
		"revision", "candidate_json", "source_sequence")
}

func TestOpenMigratesV3UsageModelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v3Schema := strings.ReplaceAll(
		schemaCurrent,
		"    model_metadata_json TEXT NOT NULL DEFAULT '{}',\n",
		"",
	)
	v3Schema = strings.Replace(
		v3Schema,
		"    CHECK (json_valid(model_metadata_json)),\n",
		"",
		1,
	)
	v3Schema = strings.Replace(
		v3Schema,
		"    updated_at TEXT NOT NULL,\n    CHECK (json_valid(model_metadata_json))\n",
		"    updated_at TEXT NOT NULL\n",
		1,
	)
	if _, err := raw.ExecContext(t.Context(), v3Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		t.Context(),
		"PRAGMA user_version = 3",
	); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	assertPragma(t, migrated.DB(), "user_version", "4")
	assertTableColumns(t, migrated.DB(), "usage", "model_metadata_json")
	assertTableColumns(
		t,
		migrated.DB(),
		"usage_turn_context",
		"model_metadata_json",
	)
}

func TestTransactionCommitRollbackAndForeignKeys(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	err := store.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO workspaces(id, root_path, created_at, updated_at)
			VALUES ('workspace-1', '/workspace', ?, ?)`, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	rollbackMarker := errors.New("rollback")
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
			VALUES ('session-rollback', 'workspace-1', 'open', ?, ?)`, now, now); err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("transaction error = %v, want rollback marker", err)
	}
	var count int
	if err := store.DB().QueryRowContext(
		ctx, "SELECT count(*) FROM sessions WHERE id = 'session-rollback'",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled-back rows = %d, want 0", count)
	}

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		VALUES ('orphan', 'missing', 'open', ?, ?)`, now, now)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("foreign-key insert error = %v", err)
	}
}

func TestBusyTimeoutAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	timeout := 90 * time.Millisecond
	first, err := Open(t.Context(), path, Options{BusyTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(t.Context(), path, Options{BusyTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	tx, err := first.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(t.Context(), `
		INSERT INTO workspaces(id, root_path, created_at, updated_at)
		VALUES ('held', '/held', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	started := time.Now()
	_, err = second.DB().ExecContext(t.Context(), `
		INSERT INTO workspaces(id, root_path, created_at, updated_at)
		VALUES ('blocked', '/blocked', ?, ?)`, now, now)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("busy write error = %v", err)
	}
	if elapsed < timeout/2 {
		t.Fatalf("busy write returned after %s, expected wait near %s", elapsed, timeout)
	}
}

func TestOpenRejectsUnsupportedSchemaWithoutChangingIt(t *testing.T) {
	for _, version := range []int{2, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsupported.db")
			store, err := Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(
				t.Context(),
				fmt.Sprintf("PRAGMA user_version = %d", version),
			); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			_, err = Open(t.Context(), path)
			var versionErr *SchemaVersionError
			if !errors.As(err, &versionErr) {
				t.Fatalf("Open error = %v, want SchemaVersionError", err)
			}
			if versionErr.Found != version ||
				versionErr.Supported != SchemaVersion {
				t.Fatalf("schema error = %+v", versionErr)
			}
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf(
					"schema error does not wrap ErrUnsupportedSchema: %v",
					err,
				)
			}
			if version < SchemaVersion &&
				!strings.Contains(err.Error(), "older than required") {
				t.Fatalf("older schema error = %v", err)
			}
			if version > SchemaVersion &&
				!strings.Contains(err.Error(), "newer than supported") {
				t.Fatalf("newer schema error = %v", err)
			}

			raw, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			assertPragma(t, raw, "user_version", fmt.Sprint(version))
		})
	}
}

func TestOpenReportsCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(t.Context(), path)
	var corruption *CorruptionError
	if !errors.As(err, &corruption) {
		t.Fatalf("Open error = %v, want CorruptionError", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption error does not wrap ErrCorrupt: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	store := openTestStore(t, Options{})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertPragma(t *testing.T, db *sql.DB, pragma, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

func assertTableColumns(t *testing.T, db *sql.DB, table string, want ...string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range want {
		if !found[column] {
			t.Errorf("table %q is missing column %q", table, column)
		}
	}
}

func TestIsUniqueConstraintViolationUsesSQLiteCodes(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.ExecContext(t.Context(), `
		CREATE TABLE error_class_parent(id TEXT PRIMARY KEY);
		CREATE TABLE error_class_child(
			id TEXT PRIMARY KEY,
			parent_id TEXT REFERENCES error_class_parent(id)
		);
		INSERT INTO error_class_parent(id) VALUES ('parent');
	`); err != nil {
		t.Fatal(err)
	}
	_, duplicateErr := db.ExecContext(
		t.Context(), `INSERT INTO error_class_parent(id) VALUES ('parent')`,
	)
	if !IsUniqueConstraintViolation(duplicateErr) {
		t.Fatalf("duplicate error was not classified: %v", duplicateErr)
	}
	_, foreignKeyErr := db.ExecContext(
		t.Context(), `INSERT INTO error_class_child(id, parent_id) VALUES ('child', 'missing')`,
	)
	if foreignKeyErr == nil {
		t.Fatal("expected foreign key error")
	}
	if IsUniqueConstraintViolation(foreignKeyErr) {
		t.Fatalf("foreign key error was classified as unique: %v", foreignKeyErr)
	}
}

func TestSchemaMigrationChainIsContiguous(t *testing.T) {
	if len(schemaMigrations) == 0 {
		t.Fatal("schema migration chain is empty")
	}
	seen := make(map[int]bool)
	for index, step := range schemaMigrations {
		if step.from >= step.to {
			t.Fatalf("migration %d does not advance: %d -> %d", index, step.from, step.to)
		}
		if seen[step.from] {
			t.Fatalf("duplicate migration source version %d", step.from)
		}
		seen[step.from] = true
		if step.run == nil {
			t.Fatalf("migration %d -> %d has no run function", step.from, step.to)
		}
		if index == 0 {
			continue
		}
		if step.from != schemaMigrations[index-1].to {
			t.Fatalf(
				"migration chain is not contiguous: %d -> %d then %d -> %d",
				schemaMigrations[index-1].from, schemaMigrations[index-1].to,
				step.from, step.to,
			)
		}
	}
	last := schemaMigrations[len(schemaMigrations)-1]
	if last.to != SchemaVersion {
		t.Fatalf("migration chain ends at %d, want SchemaVersion %d", last.to, SchemaVersion)
	}
}
