package sqlkit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestWithTxCommitRollbackAndPanic(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE values_table(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := WithTx(t.Context(), db, nil, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO values_table VALUES ('commit')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop")
	if err := WithTx(t.Context(), db, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO values_table VALUES ('rollback')`); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("rollback error = %v", err)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != "panic sentinel" {
				t.Fatalf("panic = %v", recovered)
			}
		}()
		_ = WithTx(t.Context(), db, nil, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO values_table VALUES ('panic')`); err != nil {
				return err
			}
			panic("panic sentinel")
		})
	}()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM values_table`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted rows = %d, want 1", count)
	}
}

func TestWithTxRejectsInvalidInputsAndCanceledContext(t *testing.T) {
	db := openTestDB(t)
	if err := WithTx(t.Context(), nil, nil, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("nil database succeeded")
	}
	if err := WithTx(t.Context(), db, nil, nil); err == nil {
		t.Fatal("nil callback succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := WithTx(ctx, db, nil, func(*sql.Tx) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transaction error = %v", err)
	}
}

func TestWithTxJoinsRollbackErrorAndReturnsCommitErrorUnwrapped(t *testing.T) {
	callbackErr := errors.New("callback failed")
	rollbackErr := errors.New("rollback failed")
	db := openFaultDB(t, &faultTx{rollbackErr: rollbackErr})
	err := WithTx(t.Context(), db, nil, func(*sql.Tx) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("joined transaction error = %v", err)
	}

	commitErr := errors.New("commit failed")
	db = openFaultDB(t, &faultTx{commitErr: commitErr})
	if err := WithTx(t.Context(), db, nil, func(*sql.Tx) error {
		return nil
	}); err != commitErr {
		t.Fatalf("commit error = %v, want original %v", err, commitErr)
	}
}

func TestScanAllAndRequireAffected(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE values_table(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO values_table VALUES ('a'), ('b')`)
	if err != nil {
		t.Fatal(err)
	}
	if affectedErr := RequireAffected(result, 2); affectedErr != nil {
		t.Fatal(affectedErr)
	}
	if affectedErr := RequireAffected(result, 1); affectedErr == nil {
		t.Fatal("wrong affected count succeeded")
	} else {
		var mismatch *AffectedRowsError
		if !errors.As(affectedErr, &mismatch) ||
			mismatch.Actual != 2 ||
			mismatch.Expected != 1 {
			t.Fatalf("affected rows error = %#v", affectedErr)
		}
	}
	rows, err := db.Query(`SELECT value FROM values_table ORDER BY value`)
	if err != nil {
		t.Fatal(err)
	}
	values, err := ScanAll(rows, func(row RowScanner) (string, error) {
		var value string
		scanErr := row.Scan(&value)
		return value, scanErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Fatalf("values = %#v", values)
	}
}

func TestCanonicalNullableAndTimestamp(t *testing.T) {
	object, err := CanonicalObject(json.RawMessage(`{ "b": 2, "a": 1 }`))
	if err != nil {
		t.Fatal(err)
	}
	if string(object) != `{"a":1,"b":2}` {
		t.Fatalf("object = %s", object)
	}
	if _, canonicalErr := CanonicalObject(json.RawMessage(`[]`)); canonicalErr == nil {
		t.Fatal("array accepted as object")
	}
	value, err := CanonicalJSON(json.RawMessage(` [ 1, true ] `))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != `[1,true]` {
		t.Fatalf("value = %s", value)
	}
	large, err := CanonicalJSON(json.RawMessage(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(large) != `{"id":9007199254740993}` {
		t.Fatalf("large integer lost precision: %s", large)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"ok":true} {"extra":true}`),
		json.RawMessage(`null`),
	} {
		if _, canonicalErr := CanonicalObject(raw); canonicalErr == nil {
			t.Fatalf("invalid object accepted: %s", raw)
		}
	}
	if NullableString("") != nil || NullableString("x") != "x" {
		t.Fatal("nullable string contract failed")
	}
	at := time.Date(2026, 8, 12, 10, 0, 0, 123, time.FixedZone("offset", 3600))
	if Timestamp(at) != "2026-08-12T09:00:00.000000123Z" {
		t.Fatalf("timestamp = %s", Timestamp(at))
	}
	if NullableTime(nil) != nil || NullableTime(&at) != Timestamp(at) {
		t.Fatal("nullable time contract failed")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var faultDriverSequence atomic.Uint64

type faultDriver struct {
	tx *faultTx
}

func (d *faultDriver) Open(string) (driver.Conn, error) {
	return &faultConn{tx: d.tx}, nil
}

type faultConn struct {
	tx *faultTx
}

func (*faultConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}

func (*faultConn) Close() error { return nil }

func (c *faultConn) Begin() (driver.Tx, error) { return c.tx, nil }

func (c *faultConn) BeginTx(
	context.Context,
	driver.TxOptions,
) (driver.Tx, error) {
	return c.tx, nil
}

type faultTx struct {
	commitErr   error
	rollbackErr error
}

func (t *faultTx) Commit() error   { return t.commitErr }
func (t *faultTx) Rollback() error { return t.rollbackErr }

func openFaultDB(t *testing.T, tx *faultTx) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("sqlkit-fault-%d", faultDriverSequence.Add(1))
	sql.Register(name, &faultDriver{tx: tx})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
