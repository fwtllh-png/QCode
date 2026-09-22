package turnstate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Store struct {
	database *sqlitestate.Store
	now      func() time.Time
}

func NewSQLiteRepository(database *sqlitestate.Store) *Store {
	return &Store{database: database, now: time.Now}
}

func (s *Store) AppendDomainFacts(
	ctx context.Context,
	turnID string,
	expectedNext uint64,
	facts []turnkernel.DomainFact,
) error {
	if s == nil || s.database == nil || turnID == "" || len(facts) == 0 {
		return errors.New("domain fact append is incomplete")
	}
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		return s.appendDomainFactsTx(
			ctx,
			tx,
			turnID,
			expectedNext,
			facts,
			false,
		)
	})
}

// AppendDomainFactsIdempotentTx appends a fact batch within a caller-owned
// transaction. An exact previously committed batch is accepted so a rebase can
// be retried after its transaction committed but before the caller observed it.
func (s *Store) AppendDomainFactsIdempotentTx(
	ctx context.Context,
	tx *sql.Tx,
	turnID string,
	expectedNext uint64,
	facts []turnkernel.DomainFact,
) error {
	if s == nil || s.database == nil || tx == nil ||
		turnID == "" || len(facts) == 0 {
		return errors.New("domain fact append is incomplete")
	}
	return s.appendDomainFactsTx(
		ctx,
		tx,
		turnID,
		expectedNext,
		facts,
		true,
	)
}

// AppendVerifiedDomainFacts appends a coordinator-verified batch: turnkernel
// computed the digests and canonical encodings once alongside the state, so
// the store skips the per-fact digest recomputation and the durable-tail
// decode. Sequence contiguity, terminal rejection, and digest-chain anchoring
// against the durable tail are still enforced, and decode re-verifies every
// stored fact.
func (s *Store) AppendVerifiedDomainFacts(
	ctx context.Context,
	batch turnkernel.DomainFactBatch,
) error {
	if s == nil || s.database == nil || batch.TurnID == "" ||
		len(batch.Facts) == 0 || batch.StateEncoding == nil {
		return errors.New("verified domain fact append is incomplete")
	}
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		return s.appendVerifiedDomainFactsTx(ctx, tx, batch)
	})
}

func (s *Store) appendVerifiedDomainFactsTx(
	ctx context.Context,
	tx *sql.Tx,
	batch turnkernel.DomainFactBatch,
) error {
	turnID := batch.TurnID
	expectedNext := batch.ExpectedNext
	var count, lastSequence uint64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(MAX(sequence), 0)
		 FROM turn_domain_facts WHERE turn_id = ?`,
		turnID,
	).Scan(&count, &lastSequence); err != nil {
		return err
	}
	if count != lastSequence {
		return errors.New("domain fact sequence is not contiguous")
	}
	if expectedNext != count+1 {
		return fmt.Errorf(
			"domain fact sequence conflict: got %d want %d",
			expectedNext,
			count+1,
		)
	}
	var terminal int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM turn_terminal_envelopes WHERE turn_id = ?`,
		turnID,
	).Scan(&terminal); err != nil {
		return err
	}
	if terminal != 0 {
		return errors.New("terminal turn rejects new domain facts")
	}
	if count == 0 {
		if batch.PreviousDigest != "" || batch.PreviousEncoding != nil {
			return errors.New(
				"verified append anchors to an empty fact tail",
			)
		}
	} else {
		if batch.PreviousEncoding == nil || batch.PreviousDigest == "" {
			return errors.New(
				"verified append is missing its tail anchor",
			)
		}
		// One row read anchors the batch to the durable chain, guarding
		// against an in-memory state that diverged from storage.
		var encoded []byte
		if err := tx.QueryRowContext(
			ctx,
			`SELECT fact_json FROM turn_domain_facts
			 WHERE turn_id = ? AND sequence = ?`,
			turnID,
			count,
		).Scan(&encoded); err != nil {
			return err
		}
		durableDigest, err := decodeStoredFactDigest(encoded)
		if err != nil {
			return err
		}
		if durableDigest != batch.PreviousDigest {
			return fmt.Errorf(
				"verified append diverges from durable digest at sequence %d",
				count,
			)
		}
	}
	previousDigest := batch.PreviousDigest
	previousEncoding := batch.PreviousEncoding
	for index, fact := range batch.Facts {
		if fact.TurnID != turnID ||
			fact.Sequence != expectedNext+uint64(index) {
			return fmt.Errorf("invalid domain fact at index %d", index)
		}
		if fact.StateDigest == "" {
			return fmt.Errorf(
				"domain fact digest is missing at index %d",
				index,
			)
		}
		encoded, err := encodeVerifiedDomainFact(
			fact,
			batch.StateEncoding,
			previousEncoding,
			previousDigest,
		)
		if err != nil {
			return err
		}
		encoded, err = persistFactContent(ctx, tx, fact, encoded)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO turn_domain_facts(turn_id, sequence, fact_json)
			 VALUES (?, ?, ?)`,
			turnID,
			fact.Sequence,
			string(encoded),
		); err != nil {
			return err
		}
		previousEncoding = batch.StateEncoding
		previousDigest = fact.StateDigest
	}
	return nil
}

func (s *Store) appendDomainFactsTx(
	ctx context.Context,
	tx *sql.Tx,
	turnID string,
	expectedNext uint64,
	facts []turnkernel.DomainFact,
	allowReplay bool,
) error {
	var count, lastSequence uint64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(MAX(sequence), 0)
		 FROM turn_domain_facts WHERE turn_id = ?`,
		turnID,
	).Scan(&count, &lastSequence); err != nil {
		return err
	}
	if count != lastSequence {
		return errors.New("domain fact sequence is not contiguous")
	}
	if allowReplay && expectedNext > 0 &&
		count >= expectedNext+uint64(len(facts))-1 {
		encoded, err := loadEncodedFactsForReplay(ctx, tx, turnID, expectedNext)
		if err != nil {
			return err
		}
		existing, err := decodeDomainFacts(encoded)
		if err != nil {
			return err
		}
		bySequence := make(map[uint64]turnkernel.DomainFact, len(existing))
		for _, fact := range existing {
			bySequence[fact.Sequence] = fact
		}
		for index, fact := range facts {
			stored, ok := bySequence[expectedNext+uint64(index)]
			if !ok {
				return errors.New("domain fact replay is incomplete")
			}
			left, marshalErr := json.Marshal(stored)
			if marshalErr != nil {
				return marshalErr
			}
			right, marshalErr := json.Marshal(fact)
			if marshalErr != nil || !bytes.Equal(left, right) {
				return errors.New("domain fact replay conflicts with stored state")
			}
		}
		return nil
	}
	if expectedNext != count+1 {
		return fmt.Errorf(
			"domain fact sequence conflict: got %d want %d",
			expectedNext,
			count+1,
		)
	}
	var terminal int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM turn_terminal_envelopes WHERE turn_id = ?`,
		turnID,
	).Scan(&terminal); err != nil {
		return err
	}
	if terminal != 0 {
		return errors.New("terminal turn rejects new domain facts")
	}
	var previous *turnkernel.State
	var previousDigest string
	if count != 0 {
		snapshotSequence :=
			((count - 1) / domainFactSnapshotEvery *
				domainFactSnapshotEvery) + 1
		if snapshotSequence > 1 {
			var encodedPrevious []byte
			if err := tx.QueryRowContext(
				ctx,
				`SELECT fact_json FROM turn_domain_facts
				 WHERE turn_id = ? AND sequence = ?`,
				turnID,
				snapshotSequence-1,
			).Scan(&encodedPrevious); err != nil {
				return err
			}
			digest, err := decodeStoredFactDigest(encodedPrevious)
			if err != nil {
				return err
			}
			previousDigest = digest
		}
		encodedTail, err := loadEncodedFactsFrom(
			ctx,
			tx,
			turnID,
			snapshotSequence,
		)
		if err != nil {
			return err
		}
		tail, err := decodeDomainFactSuffix(
			encodedTail,
			snapshotSequence,
			previousDigest,
		)
		if err != nil {
			return err
		}
		if len(tail) == 0 || tail[len(tail)-1].Sequence != count {
			return errors.New("domain fact tail is incomplete")
		}
		state := tail[len(tail)-1].State
		previous = &state
		previousDigest = tail[len(tail)-1].StateDigest
	}
	for index, fact := range facts {
		if fact.TurnID != turnID ||
			fact.Sequence != expectedNext+uint64(index) {
			return fmt.Errorf("invalid domain fact at index %d", index)
		}
		digest, err := turnkernel.Digest(fact.State)
		if err != nil || digest != fact.StateDigest {
			return fmt.Errorf("domain fact digest mismatch at index %d", index)
		}
		encoded, err := encodeDomainFact(
			fact,
			previous,
			previousDigest,
		)
		if err != nil {
			return err
		}
		encoded, err = persistFactContent(ctx, tx, fact, encoded)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO turn_domain_facts(turn_id, sequence, fact_json)
			 VALUES (?, ?, ?)`,
			turnID,
			fact.Sequence,
			string(encoded),
		); err != nil {
			return err
		}
		state := fact.State
		previous = &state
		previousDigest = fact.StateDigest
	}
	return nil
}

func (s *Store) LoadDomainFacts(
	ctx context.Context,
	turnID string,
) ([]turnkernel.DomainFact, error) {
	encoded, err := loadEncodedFacts(ctx, s.database.DB(), turnID)
	if err != nil {
		return nil, err
	}
	return decodeDomainFacts(encoded)
}

func (s *Store) CommitTerminal(
	ctx context.Context,
	envelope turnkernel.TerminalEnvelope,
) (turnkernel.TerminalCommitMarker, error) {
	return s.commitTerminal(ctx, envelope, false)
}

func (s *Store) CommitTerminalOperation(
	ctx context.Context,
	envelope turnkernel.TerminalEnvelope,
) (turnkernel.TerminalCommitMarker, error) {
	if !json.Valid(envelope.OperationCommit.Receipt) {
		return turnkernel.TerminalCommitMarker{},
			errors.New("operation commit receipt is invalid")
	}
	return s.commitTerminal(ctx, envelope, true)
}

func (s *Store) commitTerminal(
	ctx context.Context,
	envelope turnkernel.TerminalEnvelope,
	commitOperation bool,
) (turnkernel.TerminalCommitMarker, error) {
	digest, err := turnkernel.ValidateTerminalEnvelope(envelope)
	if err != nil {
		return turnkernel.TerminalCommitMarker{}, err
	}
	var marker turnkernel.TerminalCommitMarker
	err = s.database.Transaction(ctx, func(tx *sql.Tx) error {
		var existingMarker string
		err := tx.QueryRowContext(
			ctx,
			`SELECT marker_json FROM turn_terminal_envelopes WHERE turn_id = ?`,
			envelope.TurnID,
		).Scan(&existingMarker)
		switch {
		case err == nil:
			if err := json.Unmarshal([]byte(existingMarker), &marker); err != nil {
				return err
			}
			if marker.Digest != digest || marker.EffectID != envelope.EffectID {
				return turnkernel.ErrTerminalEnvelopeConflict
			}
			if commitOperation {
				return commitOperationTx(
					ctx,
					tx,
					envelope.TurnID,
					envelope.OperationCommit,
				)
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		encodedFacts, err := loadEncodedFacts(ctx, tx, envelope.TurnID)
		if err != nil {
			return err
		}
		existingFacts, err := decodeDomainFacts(encodedFacts)
		if err != nil {
			return err
		}
		count := len(existingFacts)
		if count > len(envelope.DomainFacts) {
			return turnkernel.ErrTerminalEnvelopeConflict
		}
		for index, existing := range existingFacts {
			left, marshalErr := json.Marshal(existing)
			if marshalErr != nil {
				return marshalErr
			}
			right, marshalErr := json.Marshal(envelope.DomainFacts[index])
			if marshalErr != nil || !bytes.Equal(left, right) {
				return turnkernel.ErrTerminalEnvelopeConflict
			}
		}
		var previous *turnkernel.State
		var previousDigest string
		if count != 0 {
			state := existingFacts[count-1].State
			previous = &state
			previousDigest = existingFacts[count-1].StateDigest
		}
		for index := count; index < len(envelope.DomainFacts); index++ {
			fact := envelope.DomainFacts[index]
			encoded, marshalErr := encodeDomainFact(
				fact,
				previous,
				previousDigest,
			)
			if marshalErr != nil {
				return marshalErr
			}
			encoded, marshalErr = persistFactContent(ctx, tx, fact, encoded)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO turn_domain_facts(turn_id, sequence, fact_json)
				 VALUES (?, ?, ?)`,
				envelope.TurnID,
				fact.Sequence,
				string(encoded),
			); err != nil {
				return err
			}
			state := fact.State
			previous = &state
			previousDigest = fact.StateDigest
		}
		marker = turnkernel.TerminalCommitMarker{
			TurnID: envelope.TurnID, EffectID: envelope.EffectID,
			Digest: digest, CommittedAt: s.now().UTC(),
		}
		storedEnvelope := envelope
		storedEnvelope.DomainFacts = nil
		envelopeJSON, err := json.Marshal(storedEnvelope)
		if err != nil {
			return err
		}
		envelopeJSON, err = externalizeContent(ctx, tx, envelopeJSON, "terminal", envelope.TurnID)
		if err != nil {
			return err
		}
		if err := bindTerminalContext(ctx, tx, envelope); err != nil {
			return err
		}
		markerJSON, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO turn_terminal_envelopes(
				turn_id, effect_id, digest, envelope_json, marker_json
			) VALUES (?, ?, ?, ?, ?)`,
			envelope.TurnID,
			envelope.EffectID,
			digest,
			string(envelopeJSON),
			string(markerJSON),
		); err != nil {
			return err
		}
		for _, entry := range envelope.Outbox {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO turn_terminal_outbox(turn_id, entry_id)
				 VALUES (?, ?)`,
				envelope.TurnID,
				entry.ID,
			); err != nil {
				return err
			}
		}
		if commitOperation {
			if err := commitOperationTx(
				ctx,
				tx,
				envelope.TurnID,
				envelope.OperationCommit,
			); err != nil {
				return err
			}
		}
		return nil
	})
	return marker, err
}

func commitOperationTx(
	ctx context.Context,
	tx *sql.Tx,
	turnID string,
	fact turnkernel.OperationCommitFact,
) error {
	var status string
	var response sql.NullString
	err := tx.QueryRowContext(
		ctx,
		`SELECT status, response_json FROM operations WHERE id = ?`,
		fact.OperationID,
	).Scan(&status, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("terminal operation is missing")
	}
	if err != nil {
		return err
	}
	if status == "committed" {
		if response.Valid && response.String == string(fact.Receipt) {
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE operations
			SET response_json = ?, updated_at = ?
			WHERE id = ? AND status = 'committed'
				AND EXISTS (
					SELECT 1 FROM turns
					WHERE id = ? AND operation_id = operations.id
						AND status = 'active'
				)`,
			string(fact.Receipt),
			time.Now().UTC().Format(time.RFC3339Nano),
			fact.OperationID,
			turnID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return turnkernel.ErrTerminalEnvelopeConflict
		}
		return nil
	}
	if status != "accepted" {
		return errors.New("terminal operation is not accepted")
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = 'committed', response_json = ?, updated_at = ?
		 WHERE id = ? AND status = 'accepted'`,
		string(fact.Receipt),
		time.Now().UTC().Format(time.RFC3339Nano),
		fact.OperationID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("terminal operation commit conflict")
	}
	return nil
}

func (s *Store) LoadTerminal(
	ctx context.Context,
	turnID string,
) (
	turnkernel.TerminalEnvelope,
	turnkernel.TerminalCommitMarker,
	error,
) {
	var envelopeJSON, markerJSON string
	err := s.database.DB().QueryRowContext(
		ctx,
		`SELECT envelope_json, marker_json
		 FROM turn_terminal_envelopes WHERE turn_id = ?`,
		turnID,
	).Scan(&envelopeJSON, &markerJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return turnkernel.TerminalEnvelope{},
			turnkernel.TerminalCommitMarker{},
			turnkernel.ErrTerminalEnvelopeMissing
	}
	if err != nil {
		return turnkernel.TerminalEnvelope{}, turnkernel.TerminalCommitMarker{}, err
	}
	var envelope turnkernel.TerminalEnvelope
	var marker turnkernel.TerminalCommitMarker
	hydrated, err := hydrateContent(ctx, s.database.DB(), []byte(envelopeJSON))
	if err != nil {
		return envelope, marker, err
	}
	if err := json.Unmarshal(hydrated, &envelope); err != nil {
		return envelope, marker, err
	}
	if err := json.Unmarshal([]byte(markerJSON), &marker); err != nil {
		return envelope, marker, err
	}
	if len(envelope.DomainFacts) == 0 {
		envelope.DomainFacts, err = s.LoadDomainFacts(ctx, turnID)
		if err != nil {
			return envelope, marker, err
		}
	}
	digest, err := turnkernel.ValidateTerminalEnvelope(envelope)
	if err != nil {
		return envelope, marker, err
	}
	if digest != marker.Digest {
		return envelope, marker, turnkernel.ErrTerminalEnvelopeConflict
	}
	return envelope, marker, nil
}

func (s *Store) LatestSessionDelta(
	ctx context.Context,
	threadID protocol.ThreadID,
) (json.RawMessage, error) {
	var delta string
	err := s.database.DB().QueryRowContext(
		ctx,
		`SELECT json_extract(envelope_json, '$.session_delta')
		 FROM turn_terminal_envelopes
		 WHERE EXISTS (
		   SELECT 1
		   FROM json_each(envelope_json, '$.outbox')
		   WHERE json_extract(value, '$.thread_id') = ?
		 )
		 AND json_type(envelope_json, '$.session_delta') IS NOT NULL
		 ORDER BY rowid DESC LIMIT 1`,
		threadID,
	).Scan(&delta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(delta)) {
		return nil, errors.New("stored session delta is invalid")
	}
	return json.RawMessage(delta), nil
}

func (s *Store) PendingOutbox(
	ctx context.Context,
	turnID string,
) ([]turnkernel.ProjectionOutboxEntry, error) {
	envelope, _, err := s.LoadTerminal(ctx, turnID)
	if err != nil {
		return nil, err
	}
	rows, err := s.database.DB().QueryContext(
		ctx,
		`SELECT entry_id FROM turn_terminal_outbox
		 WHERE turn_id = ? AND published = 0`,
		turnID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pending := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		pending[id] = true
	}
	var entries []turnkernel.ProjectionOutboxEntry
	for _, entry := range envelope.Outbox {
		if pending[entry.ID] {
			entries = append(entries, entry)
		}
	}
	return entries, rows.Err()
}

func (s *Store) PendingTerminalProjections(
	ctx context.Context,
) ([]turnkernel.PendingTerminalProjection, error) {
	rows, err := s.database.DB().QueryContext(
		ctx,
		`SELECT DISTINCT turn_id FROM turn_terminal_outbox
		 WHERE published = 0 ORDER BY turn_id`,
	)
	if err != nil {
		return nil, err
	}
	var turnIDs []string
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		turnIDs = append(turnIDs, turnID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	projections := make([]turnkernel.PendingTerminalProjection, 0, len(turnIDs))
	for _, turnID := range turnIDs {
		envelope, _, err := s.LoadTerminal(ctx, turnID)
		if err != nil {
			return nil, err
		}
		entries, err := s.PendingOutbox(ctx, turnID)
		if err != nil {
			return nil, err
		}
		if len(entries) != 0 {
			projections = append(
				projections,
				turnkernel.PendingTerminalProjection{
					Envelope: envelope,
					Entries:  entries,
				},
			)
		}
	}
	return projections, nil
}

func (s *Store) MarkOutboxPublished(
	ctx context.Context,
	turnID string,
	entryIDs []string,
) error {
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		for _, entryID := range entryIDs {
			result, err := tx.ExecContext(
				ctx,
				`UPDATE turn_terminal_outbox SET published = 1
				 WHERE turn_id = ? AND entry_id = ?`,
				turnID,
				entryID,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf("unknown outbox entry %q", entryID)
			}
		}
		return nil
	})
}
