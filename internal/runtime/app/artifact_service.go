package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type ArtifactService struct{ runtime *Runtime }

func stableArtifactID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:])
}

func boundedArtifactText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (r *ArtifactService) PersistSessionArtifact(
	ctx context.Context,
	event protocol.Event,
) {
	if r.runtime.sessionArtifacts == nil || r.runtime.sessionLifecycle == nil || r.runtime.profiles == nil {
		return
	}
	switch data := event.Data.(type) {
	case *protocol.PlanDeltaData:
		if data.ArtifactID == "" {
			return
		}
		sessionID, err := r.runtime.sessionLifecycle.SessionForThread(
			ctx,
			event.ThreadID,
		)
		if err != nil {
			r.LogArtifactError("resolve Plan Session", event, err)
			return
		}
		profile, err := r.runtime.profiles.Profile(ctx, sessionID, r.runtime.defaultProfile)
		if err != nil {
			r.LogArtifactError("resolve Plan Profile", event, err)
			return
		}
		executionProfileDigest, err := PlanExecutionProfileDigest(profile)
		if err != nil {
			r.LogArtifactError("digest Plan Profile", event, err)
			return
		}
		_, err = r.runtime.sessionArtifacts.SavePlan(ctx, protocol.SessionPlanArtifact{
			Version:                protocol.CheckpointProtocolVersion,
			ID:                     data.ArtifactID,
			SessionID:              sessionID,
			ThreadID:               event.ThreadID,
			TurnID:                 event.TurnID,
			Cursor:                 event.Sequence,
			Status:                 protocol.PlanArtifactReady,
			Purpose:                data.Purpose,
			Body:                   data.Body,
			ProfileRevision:        data.ProfileRevision,
			ExecutionProfileDigest: executionProfileDigest,
			CanImplement:           data.CanImplement,
			CanAutopilot:           data.CanAutopilot,
			CreatedAt:              event.CreatedAt,
		})
		if err != nil {
			r.LogArtifactError("save Plan Artifact", event, err)
		}
	case *protocol.TurnCompletedData:
		r.persistTerminalCheckpoint(
			ctx,
			event,
			protocol.CheckpointCompleted,
			data.Text,
		)
	case *protocol.TurnCanceledData:
		if protocol.NormalizeCancelReason(data.Reason) !=
			protocol.CancelReasonUserInterrupted {
			return
		}
		r.persistTerminalCheckpoint(
			ctx,
			event,
			protocol.CheckpointInterrupted,
			"Interrupted by the user; safe paired history was retained",
		)
	}
}

func (r *ArtifactService) PersistTerminalArtifactForTurn(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) {
	if r.runtime.sessionArtifacts == nil {
		return
	}
	events, err := r.replayArtifactTurn(ctx, turnID)
	if err != nil {
		r.LogArtifactError(
			"replay terminal event for Checkpoint",
			protocol.Event{ThreadID: threadID, TurnID: turnID},
			err,
		)
		return
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.ThreadID == threadID && event.TurnID == turnID &&
			protocol.IsTerminalEvent(event.Kind) {
			r.PersistSessionArtifact(ctx, event)
			return
		}
	}
}
