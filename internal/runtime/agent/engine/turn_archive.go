package engine

import (
	"context"
	"maps"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

// TurnTranscriptArchive recovers the durable transcript of a closed turn by
// its persisted turn identity. The runtime implements it over terminal
// envelopes and their staged context manifests; implementations return a nil
// slice when no durable transcript exists for the turn.
type TurnTranscriptArchive interface {
	LookupTurn(ctx context.Context, turnID string) ([]provider.Message, error)
}

// turnArchiveSnapshot is immutable for a Scope's lifetime. Tool callbacks
// must not acquire Engine.mu, which Execute holds while waiting for them.
type turnArchiveSnapshot struct {
	archive TurnTranscriptArchive
	turnIDs map[string]uint64
}

// snapshotTurnArchive runs under Engine.mu or during Engine construction.
func (e *Engine) snapshotTurnArchive() turnArchiveSnapshot {
	if e.options.TurnTranscriptArchive == nil {
		return turnArchiveSnapshot{}
	}
	return turnArchiveSnapshot{
		archive: e.options.TurnTranscriptArchive,
		turnIDs: maps.Clone(e.turnIDs),
	}
}

func (s turnArchiveSnapshot) source(turn uint64) (TurnTranscriptArchive, string) {
	if s.archive == nil {
		return nil, ""
	}
	for turnID, number := range s.turnIDs {
		if number == turn && turnID != "" {
			return s.archive, turnID
		}
	}
	return nil, ""
}
