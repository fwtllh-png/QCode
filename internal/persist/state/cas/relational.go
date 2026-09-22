package cas

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
)

// NewRelational shares the owner's connection and transaction boundary.
// Filesystem CAS remains available for independently owned artifact stores.
func NewRelational(database *sql.DB) *Store { return &Store{database: database} }

func (s *Store) ManagedOwnership() bool { return s.database != nil }

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type stageKey struct{}

// Active stages are process-owned. Opening a second handle must not discard a
// stage that is still being prepared by another Runtime in this process.
var activeStages sync.Map

func (s *Store) BeginContentStage(ctx context.Context) (context.Context, func() error) {
	if s.database == nil {
		return ctx, func() error { return nil }
	}
	id := fmt.Sprintf("%x", randomStageID())
	activeStages.Store(id, struct{}{})
	var once sync.Once
	var result error
	finish := func() error {
		once.Do(func() {
			_, result = s.database.ExecContext(context.Background(),
				"DELETE FROM content_staging WHERE stage_id = ?", id)
			activeStages.Delete(id)
			if result == nil {
				_, result = s.CollectUnreferenced(context.Background())
			}
		})
		return result
	}
	return context.WithValue(ctx, stageKey{}, id), finish
}

func randomStageID() []byte {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return value
}

// RecoverStages is called only at runtime startup, before accepting work.
func (s *Store) RecoverStages(ctx context.Context) error {
	rows, err := s.database.QueryContext(ctx, "SELECT DISTINCT stage_id FROM content_staging")
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		if _, live := activeStages.Load(id); !live {
			stale = append(stale, id)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range stale {
		if _, err := s.database.ExecContext(ctx,
			"DELETE FROM content_staging WHERE stage_id = ?", id); err != nil {
			return err
		}
	}
	return nil
}

// PutTx writes immutable bytes without an anonymous reference. The caller
// must bind a root, edge, or stage in this same transaction.
func PutTx(ctx context.Context, tx *sql.Tx, id string, data []byte) error {
	if err := validateID(id); err != nil {
		return err
	}
	if ID(data) != id {
		return ErrDigestMismatch
	}
	if data == nil {
		data = []byte{}
	}
	result, err := tx.ExecContext(ctx,
		"INSERT INTO content_objects(id, data) VALUES(?, ?) ON CONFLICT(id) DO NOTHING",
		id, data)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted != 0 {
		return err
	}
	// Deduplication must not turn an existing corrupt object into a newly
	// accepted reference. Verify stored bytes before the owner can commit.
	_, err = GetTx(ctx, tx, id)
	return err
}

func GetTx(ctx context.Context, q Queryer, id string) ([]byte, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	var data []byte
	err := q.QueryRowContext(ctx, "SELECT data FROM content_objects WHERE id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ID(data) != id {
		return nil, ErrDigestMismatch
	}
	return data, nil
}

func BindTx(ctx context.Context, tx *sql.Tx, kind, owner string, ids ...string) error {
	if kind == "" || owner == "" {
		return errors.New("content owner is incomplete")
	}
	for _, id := range ids {
		if err := validateID(id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO content_roots(owner_kind, owner_id, content_id)
			 VALUES(?, ?, ?) ON CONFLICT DO NOTHING`, kind, owner, id); err != nil {
			return err
		}
	}
	return nil
}

func LinkTx(ctx context.Context, tx *sql.Tx, parent string, children ...string) error {
	for _, child := range children {
		if parent == child {
			return errors.New("content cannot reference itself")
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO content_edges VALUES(?, ?) ON CONFLICT DO NOTHING", parent, child); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) LinkContent(ctx context.Context, parent string, children []string) error {
	if s.database == nil {
		return nil
	}
	return sqlkit.WithTx(ctx, s.database, nil, func(tx *sql.Tx) error {
		return LinkTx(ctx, tx, parent, children...)
	})
}

func (s *Store) putSQL(ctx context.Context, id string, data []byte) error {
	return sqlkit.WithTx(ctx, s.database, nil, func(tx *sql.Tx) error {
		if err := PutTx(ctx, tx, id, data); err != nil {
			return err
		}
		if stage, ok := ctx.Value(stageKey{}).(string); ok {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO content_staging VALUES(?, ?) ON CONFLICT DO NOTHING", stage, id)
			return err
		}
		_, err := tx.ExecContext(ctx,
			"UPDATE content_objects SET refs = refs + 1 WHERE id = ?", id)
		return err
	})
}

func (s *Store) adjustSQL(ctx context.Context, id string, delta int) error {
	if err := validateID(id); err != nil {
		return err
	}
	result, err := s.database.ExecContext(ctx,
		"UPDATE content_objects SET refs = max(0, refs + ?) WHERE id = ?", delta, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) referencesSQL(ctx context.Context, id string) (uint64, error) {
	var refs uint64
	err := s.database.QueryRowContext(ctx, `
		SELECT refs + (SELECT count(*) FROM content_roots WHERE content_id = o.id)
		+ (SELECT count(*) FROM content_edges WHERE child_id = o.id)
		+ (SELECT count(*) FROM content_staging WHERE content_id = o.id)
		FROM content_objects o WHERE id = ?`, id).Scan(&refs)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return refs, err
}

const reachableSQL = `WITH RECURSIVE live(id) AS (
 SELECT id FROM content_objects WHERE refs > 0
 UNION SELECT content_id FROM content_roots
 UNION SELECT content_id FROM content_staging
 UNION SELECT child_id FROM content_edges JOIN live ON parent_id = live.id
) `

// Collection and concurrent staging/binding serialize through SQLite.
// Removing unreachable edges first also collects cycles and shared subgraphs.
func CollectTx(ctx context.Context, tx *sql.Tx) (int, error) {
	if _, err := tx.ExecContext(ctx, reachableSQL+
		"DELETE FROM content_edges WHERE parent_id NOT IN (SELECT id FROM live)"); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, reachableSQL+
		"DELETE FROM content_objects WHERE id NOT IN (SELECT id FROM live)")
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (s *Store) collectSQL(ctx context.Context, id string) (int, error) {
	var count int
	err := sqlkit.WithTx(ctx, s.database, nil, func(tx *sql.Tx) error {
		if id == "" {
			var err error
			count, err = CollectTx(ctx, tx)
			return err
		}
		// A targeted collection must not delete unrelated zero-reference
		// objects whose legacy caller has not yet attached an owner.
		result, err := tx.ExecContext(ctx, reachableSQL+
			`DELETE FROM content_objects WHERE id = ? AND id NOT IN (SELECT id FROM live)
			 AND NOT EXISTS(SELECT 1 FROM content_edges WHERE child_id = ?)`, id, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		count = int(n)
		return err
	})
	return count, err
}

func (s *Store) deleteSQL(ctx context.Context, id string) error {
	// Explicit owners cannot be invalidated by an anonymous Delete.
	result, err := s.database.ExecContext(ctx,
		"DELETE FROM content_objects WHERE id = ?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}
