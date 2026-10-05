package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *ArtifactService) SessionPlan(
	ctx context.Context,
	sessionID string,
) (protocol.SessionPlanSnapshot, error) {
	if r.runtime.sessionArtifacts == nil {
		return protocol.SessionPlanSnapshot{}, runtimeProblem(protocol.CodeUnavailable, "Session Plan Artifacts are unavailable", nil)
	}
	current, err := r.runtime.SessionService.sessionState(ctx, sessionID)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	artifact, found, err := r.runtime.sessionArtifacts.LatestPlan(
		ctx,
		sessionID,
		current.ThreadID,
	)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	if !found {
		return protocol.SessionPlanSnapshot{
			Version: protocol.CheckpointProtocolVersion,
		}, nil
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return protocol.SessionPlanSnapshot{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	profile, err := r.runtime.SessionService.sessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	compatible := planProfileCompatible(artifact, profile.Profile) &&
		r.ensurePlanExecutionReady(ctx, current, artifact) == nil
	artifact.CanImplement = artifact.CanImplement && compatible
	artifact.CanAutopilot = artifact.CanAutopilot && compatible
	return protocol.SessionPlanSnapshot{
		Version:  protocol.CheckpointProtocolVersion,
		Artifact: &artifact,
	}, nil
}

func (r *ArtifactService) DecoratePlanArtifact(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	data *protocol.PlanDeltaData,
) error {
	if r.runtime.sessionArtifacts == nil || r.runtime.sessionLifecycle == nil || r.runtime.profiles == nil ||
		data == nil || !data.Done ||
		strings.TrimSpace(data.Body) == "" {
		return nil
	}
	if err := validateStructuredPlan(data.Body, false); err != nil {
		return runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan output must come from submit_plan",
			err,
		)
	}
	purpose, err := protocol.PlanPurposeFromBody(data.Body)
	if err != nil {
		return err
	}
	data.Purpose = purpose
	sessionID, err := r.runtime.sessionLifecycle.SessionForThread(ctx, threadID)
	if err != nil {
		return err
	}
	profile, err := r.runtime.profiles.Profile(ctx, sessionID, r.runtime.defaultProfile)
	if err != nil {
		return err
	}
	latest, found, err := r.runtime.sessionArtifacts.LatestPlan(ctx, sessionID, threadID)
	if err != nil {
		return err
	}
	source := []byte(data.Body)
	var document map[string]any
	if json.Unmarshal(source, &document) == nil && document["steps"] != nil {
		revision := uint64(1)
		if found {
			revision = planDocumentRevision(json.RawMessage(latest.Body)) + 1
			document["supersedes_id"] = latest.ID
		}
		document["revision"] = revision
		source, err = json.Marshal(document)
		if err != nil {
			return err
		}
		data.Body = string(source)
	}
	digest := sha256.Sum256(source)
	sourceDigest := hex.EncodeToString(digest[:])
	data.ArtifactID = stableArtifactID(
		"plan",
		sessionID,
		string(threadID),
		string(turnID),
		sourceDigest,
	)
	data.ProfileRevision = profile.Revision
	data.Status = string(protocol.PlanArtifactReady)
	data.CanImplement = purpose == protocol.PlanPurposeExecution
	data.CanAutopilot = purpose == protocol.PlanPurposeExecution
	return nil
}

func planDocumentRevision(document json.RawMessage) uint64 {
	var lineage struct {
		Revision uint64 `json:"revision"`
	}
	if json.Unmarshal(document, &lineage) != nil || lineage.Revision == 0 {
		return 1
	}
	return lineage.Revision
}
