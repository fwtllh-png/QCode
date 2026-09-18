package persistence

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestTurnWithdrawalBaselineAndTombstoneSurviveReopen(t *testing.T) {
	options := state.Options{DataDir: filepath.Join(t.TempDir(), "state")}
	store, err := state.Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.CloseAll(context.Background()) }()
	if err := EnsureThread(t.Context(), store, "thread", "session", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	repository := NewContextRebaseRepository(store)
	window, err := agentcontext.NewWindowLedger("initial", 1)
	if err != nil {
		t.Fatal(err)
	}
	before := agentcontext.ContextSnapshot{
		Epoch: 1, Revision: 1,
		History:   []provider.Message{provider.TextMessage(provider.RoleUser, "valid context")},
		Workspace: agentcontext.WorkspaceBinding{WorkspaceIdentity: "workspace:test"},
		Window:    window,
	}
	if err := before.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveTurnBaseline(t.Context(), "thread", "turn", before); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveTurnBaseline(t.Context(), "thread", "same-revision-next-turn", before); err != nil {
		t.Fatalf("distinct Turn baselines cannot share a context revision: %v", err)
	}
	other := agentcontext.CloneContextSnapshot(before)
	other.History[0] = provider.TextMessage(provider.RoleUser, "must not replace baseline")
	if err := other.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveTurnBaseline(t.Context(), "thread", "turn", other); err != nil {
		t.Fatal(err)
	}
	got, found, err := repository.TurnBaseline(t.Context(), "thread", "turn")
	if err != nil || !found || got.Digest != before.Digest {
		t.Fatalf("baseline: %+v %v", got, err)
	}
	restored := agentcontext.CloneContextSnapshot(before)
	restored.Epoch, restored.Revision = 2, 3
	if err := restored.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitTurnWithdrawal(t.Context(), "thread", "turn", restored); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitTurnWithdrawal(t.Context(), "thread", "turn", restored); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	repository = NewContextRebaseRepository(store)
	current, found, err := repository.LatestContextSnapshot(t.Context(), "thread")
	if err != nil || !found || current.Digest != restored.Digest {
		t.Fatalf("current after reopen: %+v %v", current, err)
	}
	if withdrawn, err := repository.TurnWithdrawn(t.Context(), "thread", "turn"); err != nil || !withdrawn {
		t.Fatalf("withdrawn: %v %v", withdrawn, err)
	}
	if withdrawn, err := repository.TurnWithdrawn(t.Context(), "other-thread", "turn"); err != nil || withdrawn {
		t.Fatal("cross-thread tombstone leak")
	}
	batch, err := repository.TurnsWithdrawn(t.Context(), map[protocol.ThreadID]protocol.TurnID{
		"thread": "turn", "other-thread": "turn",
	})
	if err != nil || !batch["thread"] || batch["other-thread"] {
		t.Fatalf("batch withdrawal identity: %v %v", batch, err)
	}
	batch, err = repository.TurnsWithdrawn(t.Context(), map[protocol.ThreadID]protocol.TurnID{"thread": "not-withdrawn"})
	if err != nil || batch["thread"] {
		t.Fatalf("batch withdrawal turn identity: %v %v", batch, err)
	}
	if _, found, err := repository.TurnBaseline(t.Context(), "other-thread", "turn"); err != nil || found {
		t.Fatal("cross-thread baseline leak")
	}
	invalid := restored
	invalid.Digest = "invalid"
	if err := repository.CommitTurnWithdrawal(t.Context(), "thread", "invalid", invalid); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if withdrawn, _ := repository.TurnWithdrawn(t.Context(), "thread", "invalid"); withdrawn {
		t.Fatal("failed commit left tombstone")
	}
}
