// Package sqlkit contains small, domain-neutral helpers shared by durable
// repositories. SQL text, state transitions, and domain errors stay with the
// repository that owns them.
package sqlkit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// RowScanner is implemented by *sql.Row and *sql.Rows.
type RowScanner interface {
	Scan(dest ...any) error
}

// WithTx executes fn in one transaction. It never retries or nests
// transactions.
func WithTx(
	ctx context.Context,
	db *sql.DB,
	opts *sql.TxOptions,
	fn func(*sql.Tx) error,
) (err error) {
	if db == nil {
		return errors.New("transaction database is required")
	}
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}
		if err == nil {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback transaction: %w", rollbackErr))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ScanAll consumes and closes rows, applying scan once per row.
func ScanAll[T any](
	rows *sql.Rows,
	scan func(RowScanner) (T, error),
) ([]T, error) {
	if rows == nil {
		return nil, errors.New("rows are required")
	}
	if scan == nil {
		_ = rows.Close()
		return nil, errors.New("row scanner is required")
	}
	defer rows.Close()
	values := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return values, nil
}

// CanonicalObject validates and compacts a JSON object. Empty input is the
// canonical empty object.
func CanonicalObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var value map[string]any
	if err := decodeOne(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JSON object must not be null")
	}
	return json.Marshal(value)
}

// CanonicalJSON validates and compacts any JSON value.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := decodeOne(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func decodeOne(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func NullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func NullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return Timestamp(*value)
}

func Timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// AffectedRowsError reports an optimistic or identity-bound write that
// completed but did not affect the exact number of rows promised by its owner.
type AffectedRowsError struct {
	Actual   int64
	Expected int64
}

func (e *AffectedRowsError) Error() string {
	return fmt.Sprintf("affected %d rows, expected %d", e.Actual, e.Expected)
}

// RequireAffected verifies the exact row count promised by an optimistic or
// identity-bound write.
func RequireAffected(result sql.Result, expected int64) error {
	if result == nil {
		return errors.New("SQL result is required")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != expected {
		return &AffectedRowsError{Actual: affected, Expected: expected}
	}
	return nil
}
