package turnstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestEffectContentDeduplicatesAcrossSnapshotsAndCollectsAfterDeletion(t *testing.T) {
	store := openVerifiedTestStore(t)
	payload, _ := json.Marshal(map[string]string{"input": strings.Repeat("opaque effect input", 1000)})
	sum := sha256.Sum256(payload)
	state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	state.PendingEffects["effect"] = turnkernel.Effect{
		ID: "effect", Kind: turnkernel.EffectSampleProvider, Payload: payload,
		PayloadDigest: "sha256:" + hex.EncodeToString(sum[:]), Attempt: 1,
		IdempotencyKey: "effect", Status: turnkernel.EffectRequested,
	}
	var facts []turnkernel.DomainFact
	for sequence := 1; sequence <= 35; sequence++ {
		state.Context.HistoryBytes = sequence
		digest, err := turnkernel.Digest(state)
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, turnkernel.DomainFact{
			TurnID: "content-turn", Sequence: uint64(sequence), Command: "fixture",
			State: state, StateDigest: digest,
		})
	}
	if err := store.AppendDomainFacts(t.Context(), "content-turn", 1, facts); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadDomainFacts(t.Context(), "content-turn")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored[len(restored)-1].State.PendingEffects["effect"].Payload, payload) {
		t.Fatal("effect payload changed during restoration")
	}
	var count int
	if err := store.database.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM content_objects").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("same payload stored %d times", count)
	}
	for _, raw := range storedFactRows(t, store, "content-turn") {
		if bytes.Contains(raw, []byte("opaque effect input")) {
			t.Fatal("effect payload is still inline")
		}
	}
	blobs := cas.NewRelational(store.database.DB())
	if err := store.database.Transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "DELETE FROM turn_domain_facts WHERE turn_id = 'content-turn'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := blobs.CollectUnreferenced(t.Context()); err != nil || count != 1 {
		t.Fatalf("effect collection: %d %v", count, err)
	}
}

func TestContentWritesRollbackWithDomainFactFailure(t *testing.T) {
	store := openVerifiedTestStore(t)
	envelope := sqliteEnvelopeFixture(t)
	_, err := store.database.DB().ExecContext(t.Context(), `
		CREATE TRIGGER fail_terminal BEFORE INSERT ON turn_terminal_envelopes
		BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTerminal(t.Context(), envelope); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"content_objects", "content_roots", "content_edges"} {
		var count int
		if err := store.database.DB().QueryRowContext(context.Background(), fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s leaked %d rows after rollback", table, count)
		}
	}
}

func TestContinuationStageTransfersOwnershipOnlyWithCommittedFact(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			store := openVerifiedTestStore(t)
			blobs := cas.NewRelational(store.database.DB())
			ctx, finish := blobs.BeginContentStage(t.Context())
			defer finish()
			record := agentcontext.TurnContinuation{
				Version: agentcontext.ContinuationVersion, TurnID: "continuation-turn",
				Sequence: 1, TurnNumber: 1, ProfileRevision: 1,
				WorkspaceIdentity: "workspace:test", Provider: "test", Model: "test",
				Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "accepted message")},
			}
			ref, err := agentcontext.StoreTurnContinuation(ctx, blobs, record)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := blobs.CollectUnreferenced(ctx); err != nil || n != 0 {
				t.Fatalf("unpublished content collected: %d %v", n, err)
			}
			state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
			state.Continuation = &turnkernel.ContinuationCursor{
				Sequence: record.Sequence, Handle: ref.Handle, Digest: ref.Digest,
			}
			digest, err := turnkernel.Digest(state)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := store.database.DB().ExecContext(ctx, `
					CREATE TRIGGER fail_fact BEFORE INSERT ON turn_domain_facts
					BEGIN SELECT RAISE(ABORT, 'injected fact failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = store.AppendDomainFacts(ctx, record.TurnID, 1, []turnkernel.DomainFact{{
				TurnID: record.TurnID, Sequence: 1, Command: "fixture",
				State: state, StateDigest: digest,
			}})
			if (err != nil) != fail {
				t.Fatalf("fact commit: %v, want failure=%v", err, fail)
			}
			if err := finish(); err != nil {
				t.Fatal(err)
			}
			if !fail {
				restored, err := agentcontext.LoadTurnContinuation(t.Context(), blobs, ref.Handle, ref.Digest)
				if err != nil || len(restored.Messages) != len(record.Messages) {
					t.Fatalf("committed continuation unavailable: %+v %v", restored, err)
				}
				if _, err := store.database.DB().ExecContext(t.Context(),
					"DELETE FROM turn_domain_facts WHERE turn_id = ?", record.TurnID); err != nil {
					t.Fatal(err)
				}
				if _, err := blobs.CollectUnreferenced(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			for _, table := range []string{"content_objects", "content_roots", "content_edges", "content_staging"} {
				var count int
				if err := store.database.DB().QueryRowContext(t.Context(),
					"SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s leaked after stage: %d %v", table, count, err)
				}
			}
		})
	}
}

func TestContentRoundTripPreservesRawJSONBytes(t *testing.T) {
	store := openVerifiedTestStore(t)
	raw := []byte(` { "receipt": {"z":1,"a":[{"payload": { "z":2, "a":"\u003c" }},null]}, "assembly":{"z":3,"a":4}, "payload":null } `)
	if err := store.database.Transaction(t.Context(), func(tx *sql.Tx) error {
		stored, err := externalizeContent(t.Context(), tx, raw, "test", "raw")
		if err != nil {
			return err
		}
		restored, err := hydrateContent(t.Context(), tx, stored)
		if err != nil {
			return err
		}
		if !bytes.Equal(restored, raw) {
			t.Fatalf("raw JSON changed:\ngot  %s\nwant %s", restored, raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalContentRestorationPreservesReceiptDigest(t *testing.T) {
	store := openVerifiedTestStore(t)
	envelope := sqliteEnvelopeFixture(t)
	envelope.OperationCommit.Receipt = json.RawMessage(`{"status":"committed","operation_id":"operation-sqlite"}`)
	envelope.Outbox[0].Payload = json.RawMessage(`{"z":1,"a":"receipt"}`)
	marker, err := store.CommitTerminal(t.Context(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	restored, loaded, err := store.LoadTerminal(t.Context(), envelope.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if marker != loaded || !bytes.Equal(restored.OperationCommit.Receipt, envelope.OperationCommit.Receipt) {
		t.Fatalf("terminal receipt or marker changed: %+v %+v", restored.OperationCommit, loaded)
	}
}
