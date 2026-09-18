package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func turnContextID(kind string, thread protocol.ThreadID, turn protocol.TurnID) string {
	return fmt.Sprintf("%s:%x", kind, sha256.Sum256([]byte(string(thread)+"\x00"+string(turn))))
}

func (r *ContextRebaseRepository) SaveTurnBaseline(
	ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID, snapshot agentcontext.ContextSnapshot,
) error {
	if thread == "" || turn == "" {
		return errors.New("Turn baseline identity is required")
	}
	if _, found, err := r.TurnBaseline(ctx, thread, turn); found || err != nil {
		return err
	}
	manifest, err := agentcontext.BuildContextManifest(ctx, r.store.Content(), thread, turn, snapshot, nil, agentcontext.ManifestLimits{})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	err = r.saveBaselineManifest(ctx, thread, turn, raw)
	if err != nil {
		r.releaseNewRefs(context.Background(), nil, manifest)
	}
	return err
}

func (r *ContextRebaseRepository) saveBaselineManifest(
	ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID, raw []byte,
) error {
	// Baselines have no event cursor yet and do not advance current context.
	// Scope the kind to the admitted Turn, including repeated empty baselines.
	_, err := snapshot.NewSQLiteRepository(r.store.SQLite(), r.store.Content()).Save(ctx, snapshot.Snapshot{
		ID: turnContextID("turn-baseline", thread, turn), ThreadID: thread,
		Kind: "turn-baseline:" + string(turn), Content: raw,
	})
	return err
}

func (r *ContextRebaseRepository) TurnBaseline(
	ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID,
) (agentcontext.ContextSnapshot, bool, error) {
	record, err := snapshot.NewSQLiteRepository(r.store.SQLite(), r.store.Content()).Get(ctx,
		turnContextID("turn-baseline", thread, turn))
	if errors.Is(err, snapshot.ErrNotFound) {
		return agentcontext.ContextSnapshot{}, false, nil
	}
	if err != nil {
		return agentcontext.ContextSnapshot{}, false, err
	}
	var manifest agentcontext.ContextManifest
	if err := json.Unmarshal(record.Content, &manifest); err != nil {
		return agentcontext.ContextSnapshot{}, false, err
	}
	if manifest.ThreadID != thread || manifest.TurnID != turn {
		return agentcontext.ContextSnapshot{}, false, errors.New("Turn baseline identity mismatch")
	}
	snapshot, err := agentcontext.LoadContextManifest(ctx, r.store.Content(), manifest)
	return snapshot, err == nil, err
}

func (r *ContextRebaseRepository) TurnWithdrawn(
	ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID,
) (bool, error) {
	var found bool
	err := r.store.SQLite().DB().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM context_rebases WHERE compaction_id = ? AND thread_id = ? AND turn_id = ?)`,
		turnContextID("turn-withdrawn", thread, turn), thread, turn).Scan(&found)
	return found, err
}

func (r *ContextRebaseRepository) TurnsWithdrawn(
	ctx context.Context, turns map[protocol.ThreadID]protocol.TurnID,
) (map[protocol.ThreadID]bool, error) {
	result := make(map[protocol.ThreadID]bool, len(turns))
	if len(turns) == 0 {
		return result, nil
	}
	type identity struct {
		ID     string            `json:"id"`
		Thread protocol.ThreadID `json:"thread"`
		Turn   protocol.TurnID   `json:"turn"`
	}
	identities := make([]identity, 0, len(turns))
	for thread, turn := range turns {
		identities = append(identities, identity{
			ID: turnContextID("turn-withdrawn", thread, turn), Thread: thread, Turn: turn,
		})
	}
	raw, err := json.Marshal(identities)
	if err != nil {
		return nil, err
	}
	rows, err := r.store.SQLite().DB().QueryContext(ctx, `
		SELECT c.thread_id FROM json_each(?) p
		JOIN context_rebases c
		  ON c.compaction_id = json_extract(p.value, '$.id')
		 AND c.thread_id = json_extract(p.value, '$.thread')
		 AND c.turn_id = json_extract(p.value, '$.turn')`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var thread protocol.ThreadID
		if err := rows.Scan(&thread); err != nil {
			return nil, err
		}
		result[thread] = true
	}
	return result, rows.Err()
}

func (r *ContextRebaseRepository) CommitTurnWithdrawal(
	ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID, snapshot agentcontext.ContextSnapshot,
) error {
	// The stable commit identity is also the withdrawal tombstone. The context
	// pointer and tombstone are committed in the same SQLite transaction.
	return r.CommitCurrentContext(ctx, agentcontext.CurrentContextCommit{
		ID:       turnContextID("turn-withdrawn", thread, turn),
		ThreadID: thread, TurnID: turn, Snapshot: snapshot,
	})
}
