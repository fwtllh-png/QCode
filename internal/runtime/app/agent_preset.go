package app

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *AgentPresetService) List(
	ctx context.Context,
	request protocol.AgentPresetListRequest,
) (protocol.AgentPresetList, error) {
	if err := request.Validate(); err != nil {
		return protocol.AgentPresetList{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	if s.agentPresets == nil {
		return protocol.AgentPresetList{}, runtimeProblem(
			protocol.CodeUnavailable,
			"agent preset storage is unavailable",
			nil,
		)
	}
	if _, err := s.SessionStatus(ctx, request.SessionID); err != nil {
		return protocol.AgentPresetList{}, err
	}
	return s.agentPresets.List(ctx)
}

func (s *AgentPresetService) Save(
	ctx context.Context,
	request protocol.AgentPresetSaveRequest,
) (protocol.AgentPresetMutationResult, error) {
	if err := request.Validate(); err != nil {
		return protocol.AgentPresetMutationResult{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	if s.agentPresets == nil {
		return protocol.AgentPresetMutationResult{}, runtimeProblem(
			protocol.CodeUnavailable,
			"agent preset storage is unavailable",
			nil,
		)
	}
	if _, err := s.SessionStatus(ctx, request.SessionID); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	if err := s.validateProfile(
		ctx,
		request.SessionID,
		request.Profile,
	); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	result, err := s.agentPresets.Save(ctx, protocol.AgentPreset{
		ID: request.ID, Name: request.Name, Description: request.Description,
		Scope: protocol.AgentPresetScopeWorkspace, Profile: request.Profile,
	}, request.ExpectedRevision)
	if err != nil {
		return protocol.AgentPresetMutationResult{}, presetStoreProblem(err)
	}
	return result, nil
}

func (s *AgentPresetService) Delete(
	ctx context.Context,
	request protocol.AgentPresetDeleteRequest,
) (protocol.AgentPresetMutationResult, error) {
	if err := request.Validate(); err != nil {
		return protocol.AgentPresetMutationResult{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	if s.agentPresets == nil {
		return protocol.AgentPresetMutationResult{}, runtimeProblem(
			protocol.CodeUnavailable,
			"agent preset storage is unavailable",
			nil,
		)
	}
	if _, err := s.SessionStatus(ctx, request.SessionID); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	result, err := s.agentPresets.Delete(
		ctx,
		request.ID,
		request.ExpectedRevision,
	)
	if err != nil {
		return protocol.AgentPresetMutationResult{}, presetStoreProblem(err)
	}
	return result, nil
}

func (s *AgentPresetService) Apply(
	ctx context.Context,
	request protocol.AgentPresetApplyRequest,
) (protocol.AgentPresetApplyResult, error) {
	if err := request.Validate(); err != nil {
		return protocol.AgentPresetApplyResult{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	if s.agentPresets == nil {
		return protocol.AgentPresetApplyResult{}, runtimeProblem(
			protocol.CodeUnavailable,
			"agent preset storage is unavailable",
			nil,
		)
	}
	summary, err := s.SessionStatus(ctx, request.SessionID)
	if err != nil {
		return protocol.AgentPresetApplyResult{}, err
	}
	if summary.ThreadID != request.ThreadID {
		return protocol.AgentPresetApplyResult{}, runtimeProblem(
			protocol.CodeConflict,
			"thread does not belong to session",
			nil,
		)
	}
	preset, err := s.agentPresets.Get(ctx, request.PresetID)
	if err != nil {
		return protocol.AgentPresetApplyResult{}, presetStoreProblem(err)
	}
	current, err := s.SessionProfile(ctx, request.SessionID)
	if err != nil {
		return protocol.AgentPresetApplyResult{}, err
	}
	if current.Profile.Revision != request.ExpectedProfileRevision {
		return protocol.AgentPresetApplyResult{}, retryableProblem(
			protocol.CodeConflict,
			"session profile changed before the preset was applied",
		)
	}
	patch := preset.Profile.Patch(current.Profile)
	update := protocol.SessionProfileUpdateResult{Profile: current.Profile}
	if !emptySessionProfilePatch(patch) {
		update, err = s.UpdateSessionProfile(
			ctx,
			request.SessionID,
			request.ThreadID,
			request.ExpectedProfileRevision,
			patch,
		)
		if err != nil {
			return protocol.AgentPresetApplyResult{}, err
		}
	}
	capabilities, err := s.capabilitiesForProfile(update.Profile)
	if err != nil {
		return protocol.AgentPresetApplyResult{}, err
	}
	restartRequired := capabilities.ModelCapabilities.SelectionMode == "restart_required"
	reason := ""
	if restartRequired {
		reason = "The selected model route requires a Runtime restart"
	}
	return protocol.AgentPresetApplyResult{
		Version: protocol.AgentPresetVersion, PresetID: preset.ID,
		ProfileUpdate: update, RestartRequired: restartRequired,
		RestartReason: reason,
	}, nil
}

func (s *AgentPresetService) validateProfile(
	ctx context.Context,
	sessionID string,
	preset protocol.AgentPresetProfile,
) error {
	if err := preset.Validate(); err != nil {
		return runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	current, err := s.SessionProfile(ctx, sessionID)
	if err != nil {
		return err
	}
	patch := preset.Patch(current.Profile)
	if emptySessionProfilePatch(patch) {
		return nil
	}
	if err := validateMutableProfilePatch(
		patch,
		current.Capabilities.MutableFields,
	); err != nil {
		return err
	}
	candidate, err := protocol.ApplySessionProfilePatch(current.Profile, patch)
	if err != nil {
		return runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	if _, err := s.capabilitiesForProfile(candidate.Profile); err != nil {
		return err
	}
	if s.toolCatalog != nil {
		catalog, err := s.SessionToolCatalog(ctx, sessionID)
		if err != nil {
			return err
		}
		available := make(map[string]bool, len(catalog.Tools))
		for _, entry := range catalog.Tools {
			available[entry.ID] = entry.Availability == "available"
		}
		for _, id := range preset.EnabledToolIDs {
			if !available[id] {
				return runtimeProblem(
					protocol.CodeInvalidArgument,
					"agent preset contains an unavailable tool",
					nil,
				)
			}
		}
	}
	return nil
}

func emptySessionProfilePatch(patch protocol.SessionProfilePatch) bool {
	return patch.Provider == nil &&
		patch.Model == nil && patch.ReasoningEffort == nil &&
		patch.EnabledToolIDs == nil && patch.ApprovalPosture == nil &&
		patch.ExecutionTarget == nil && patch.MaxSteps == nil
}

func presetStoreProblem(err error) error {
	switch {
	case errors.Is(err, protocol.ErrAgentPresetRevisionConflict):
		return retryableProblem(
			protocol.CodeConflict,
			"agent preset changed; refresh before retrying",
		)
	case errors.Is(err, protocol.ErrAgentPresetNameConflict):
		return runtimeProblem(
			protocol.CodeConflict,
			"an agent preset with this name already exists",
			err,
		)
	case errors.Is(err, protocol.ErrAgentPresetNotFound):
		return runtimeProblem(
			protocol.CodeInvalidArgument,
			"agent preset was not found",
			err,
		)
	default:
		return err
	}
}
