package cas_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

func TestRelationalOwnershipStagesSharedGraphAndRollback(t *testing.T) {
	db, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := cas.NewRelational(db.DB())
	stage, finish := store.BeginContentStage(t.Context())
	child, parent := []byte("shared child"), []byte("parent")
	for _, data := range [][]byte{child, parent} {
		if err := store.Put(stage, cas.ID(data), data); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.LinkContent(stage, cas.ID(parent), []string{cas.ID(child)}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CollectUnreferenced(t.Context()); err != nil || n != 0 {
		t.Fatalf("collected active stage: %d %v", n, err)
	}
	rollback := errors.New("rollback")
	err = db.Transaction(t.Context(), func(tx *sql.Tx) error {
		if err := cas.BindTx(t.Context(), tx, "test", "rolled-back", cas.ID(parent)); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := db.Transaction(t.Context(), func(tx *sql.Tx) error {
		if err := cas.BindTx(t.Context(), tx, "test", "first", cas.ID(parent)); err != nil {
			return err
		}
		return cas.BindTx(t.Context(), tx, "test", "second", cas.ID(child))
	}); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(t.Context(),
		"DELETE FROM content_roots WHERE owner_id = 'first'"); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CollectUnreferenced(t.Context()); err != nil || n != 1 {
		t.Fatalf("collect parent: %d %v", n, err)
	}
	if data, err := store.Get(t.Context(), cas.ID(child)); err != nil || string(data) != string(child) {
		t.Fatalf("shared child: %s %v", data, err)
	}
	if _, err := db.DB().ExecContext(t.Context(), "DELETE FROM content_roots"); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CollectUnreferenced(t.Context()); err != nil || n != 1 {
		t.Fatalf("collect child: %d %v", n, err)
	}
	if n, err := store.CollectUnreferenced(t.Context()); err != nil || n != 0 {
		t.Fatalf("repeat collection: %d %v", n, err)
	}
}

func TestRelationalRecoveryRemovesOnlyAbandonedStages(t *testing.T) {
	db, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := cas.NewRelational(db.DB())
	stage, finish := store.BeginContentStage(t.Context())
	defer finish()
	data := []byte("still preparing")
	if err := store.Put(stage, cas.ID(data), data); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(t.Context(), func(tx *sql.Tx) error {
		stale := []byte("crashed writer")
		if err := cas.PutTx(t.Context(), tx, cas.ID(stale), stale); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), "INSERT INTO content_staging VALUES('crashed', ?)", cas.ID(stale))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	second := cas.NewRelational(db.DB())
	if err := second.RecoverStages(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := second.CollectUnreferenced(t.Context()); err != nil || n != 1 {
		t.Fatalf("recover collection: %d %v", n, err)
	}
	if _, err := second.Get(t.Context(), cas.ID(data)); err != nil {
		t.Fatal(err)
	}
}

func TestRelationalDeduplicationRejectsCorruptionWithoutAcquiringReference(t *testing.T) {
	db, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := cas.NewRelational(db.DB())
	data := []byte("verified content")
	id := cas.ID(data)
	for range 2 {
		if err := store.Put(t.Context(), id, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().ExecContext(t.Context(),
		"UPDATE content_objects SET data = ? WHERE id = ?", []byte("damaged"), id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("corrupt read: %v", err)
	}
	if err := store.Put(t.Context(), id, data); !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("corrupt deduplication: %v", err)
	}
	if refs, err := store.References(t.Context(), id); err != nil || refs != 2 {
		t.Fatalf("failed put changed references: %d %v", refs, err)
	}
	empty := cas.ID(nil)
	if _, err := store.Get(t.Context(), empty); !errors.Is(err, cas.ErrNotFound) {
		t.Fatalf("missing content: %v", err)
	}
	if err := db.Transaction(t.Context(), func(tx *sql.Tx) error {
		return cas.BindTx(t.Context(), tx, "test", "missing", empty)
	}); err == nil {
		t.Fatal("owner committed with missing content")
	}
	if err := store.Put(t.Context(), empty, nil); err != nil {
		t.Fatalf("empty content put: %v", err)
	}
	if got, err := store.Get(t.Context(), empty); err != nil || len(got) != 0 {
		t.Fatalf("empty content read: %q %v", got, err)
	}
}

func TestRelationalCollectionConcurrentWithStagingAndOwnership(t *testing.T) {
	db, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := cas.NewRelational(db.DB())
	stop, collected := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				collected <- nil
				return
			default:
				if _, err := store.CollectUnreferenced(t.Context()); err != nil {
					collected <- err
					return
				}
			}
		}
	}()
	var writers sync.WaitGroup
	for index := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			stage, finish := store.BeginContentStage(t.Context())
			defer finish()
			owner := fmt.Sprintf("owner-%d", index)
			child, parent := []byte("shared child"), []byte(owner)
			for _, data := range [][]byte{child, parent} {
				if err := store.Put(stage, cas.ID(data), data); err != nil {
					t.Error(err)
					return
				}
			}
			if err := store.LinkContent(stage, cas.ID(parent), []string{cas.ID(child)}); err != nil {
				t.Error(err)
				return
			}
			if err := db.Transaction(stage, func(tx *sql.Tx) error {
				return cas.BindTx(stage, tx, "test", owner, cas.ID(parent))
			}); err != nil {
				t.Error(err)
				return
			}
			if err := finish(); err != nil {
				t.Error(err)
				return
			}
			if _, err := store.Get(t.Context(), cas.ID(child)); err != nil {
				t.Errorf("collection deleted owned child: %v", err)
			}
			if _, err := db.DB().ExecContext(t.Context(),
				"DELETE FROM content_roots WHERE owner_id = ?", owner); err != nil {
				t.Error(err)
			}
		}()
	}
	writers.Wait()
	close(stop)
	if err := <-collected; err != nil {
		t.Fatal(err)
	}
	if _, err := store.CollectUnreferenced(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM content_objects").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unowned content remains: %d %v", count, err)
	}
}
