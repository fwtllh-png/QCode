package wire

import (
	"crypto/sha256"
	"errors"
	"fmt"

	memorystore "github.com/fwtllh-png/QCode/internal/adapter/memory"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type contextRuntimeBinding struct {
	durable     *durableCoordinatorRuntime
	coordinator turnkernel.CoordinatorRuntime
}

func bindMemoryScopes(
	store *memorystore.Store,
	workspace string,
	rootID string,
) (string, error) {
	workspaceID, repositoryID, err :=
		memorystore.CanonicalScopeIdentities(workspace)
	if err != nil {
		return "", err
	}
	if rootID != "" {
		workspaceID = "workspace:" + rootID
	}
	if store != nil {
		if err := store.BindScopes(workspaceID, repositoryID); err != nil {
			return "", fmt.Errorf("bind memory scopes: %w", err)
		}
	}
	return workspaceID, nil
}

func engineSecurityPolicy(
	state *buildState,
) (*agentengine.WorkspaceTurnGate, policy.Permission) {
	var workspaceTurnGate *agentengine.WorkspaceTurnGate
	if state.security.journal != nil {
		workspaceTurnGate = agentengine.NewWorkspaceTurnGate()
	}
	posture := policy.PermissionBypass
	if state.security.runtime != nil {
		posture = state.security.runtime.PermissionValue()
	} else if state.options.Permission != "" {
		posture = policy.Permission(state.options.Permission)
	}
	return workspaceTurnGate, posture
}

func validateRouteReasoning(
	route model.ReadyRoute,
	effort string,
) error {
	if effort != "" && !route.Model().Capabilities.Reasoning {
		return errors.New("execution.reasoning_effort requires a reasoning model")
	}
	return nil
}

func effectiveReasoningEffort(route model.ReadyRoute, configured string) string {
	if configured != "" {
		return configured
	}
	return route.Model().Capabilities.DefaultReasoningEffort
}

func buildContextRuntime(
	store *state.Store,
	sessionID string,
) (contextRuntimeBinding, error) {
	if store == nil {
		return contextRuntimeBinding{
			coordinator: turnkernel.NewEphemeralCoordinatorRuntime(),
		}, nil
	}
	durable, err := newDurableCoordinatorRuntime(
		turnstate.NewSQLiteRepository(store.SQLite()),
		sessionID,
		defaultTurnCoordinatorLease,
	)
	if err != nil {
		return contextRuntimeBinding{}, err
	}
	return contextRuntimeBinding{
		durable: durable, coordinator: durable,
	}, nil
}

func engineContextPolicy(
	configuration config.Context,
) agentengine.ContextPolicy {
	compact := configuration.Compact
	view := configuration.View
	return agentengine.ContextPolicy{
		Window: agentengine.CompactWindowPolicy{
			PrepareTokens:   uint64(compact.PrepareTokens),
			AutoTokens:      uint64(compact.AutoCompactTokens),
			EmergencyTokens: uint64(compact.EmergencyTokens),
			Scope:           compact.Scope,
		},
		TruthRetention: agentcontext.RetentionPolicy{
			TruthMaxBytes:        compact.TruthMaxBytes,
			TruthMaxEntities:     compact.TruthMaxEntities,
			MandatoryMaxEntities: compact.MandatoryMaxEntities,
			FactMaxEntities:      compact.FactMaxEntities,
			FailureMaxEntities:   compact.FailureMaxEntities,
			HandleMaxEntities:    compact.HandleMaxEntities,
			OmissionSampleMaxEntities: compact.
				OmissionSampleMaxEntities,
			VerifiedChangeRetentionTurns: uint64(
				compact.VerifiedChangeRetentionTurns,
			),
		},
		SemanticNarrative: view.NarrativeMode,
		Digest:            view.Digest,
		NarrativeLimits: agentcontext.NarrativeLimits{
			MaxInputBytes:  compact.SemanticNarrativeMaxInputTokens * 4,
			MaxOutputBytes: compact.SemanticNarrativeMaxOutputTokens * 4,
			MaxItems:       compact.SemanticNarrativeMaxItems,
			ItemMaxBytes:   compact.SemanticNarrativeItemMaxBytes,
		},
		NarrativeTimeout:      compact.SemanticNarrativeTimeout,
		NarrativeRetryLimit:   compact.SemanticNarrativeRetryLimit,
		OwnerDeltaMaxSegments: compact.OwnerDeltaMaxSegments,
		OwnerDeltaMaxBytes:    compact.OwnerDeltaMaxBytes,
		RecentTailTurns:       view.RecentTailTurns,
		RecentTailMaxTokens:   uint64(view.HistoryTokenCeiling),
		KeepRecentToolResults: view.KeepRecentToolResults,
		CheckpointMaxBytes:    view.CheckpointMaxBytes,
	}
}

func memorySnapshotSource(
	store *memorystore.Store,
	configuration config.Memory,
	budget promptcontext.Budget,
) func(string) (agentengine.MemorySnapshot, error) {
	return func(query string) (agentengine.MemorySnapshot, error) {
		if store == nil {
			return agentengine.MemorySnapshot{}, nil
		}
		maxBytes := configuration.MaxPromptBytes
		if budget.MaxBytes > 0 {
			maxBytes = min(maxBytes, budget.MaxBytes)
		}
		if budget.MaxTokens > 0 {
			maxBytes = min(maxBytes, int(tokenestimate.BytesForTokens(budget.MaxTokens)))
		}
		block, selection, err := store.SelectBlock(memorystore.Query{
			Text:          query,
			MaxCandidates: configuration.MaxCandidates,
			MaxBytes:      maxBytes,
		})
		if err != nil {
			return agentengine.MemorySnapshot{}, err
		}
		digest := sha256.Sum256([]byte(block))
		return agentengine.MemorySnapshot{
			Generation:     selection.Generation,
			Body:           block,
			Source:         store.Path(),
			Digest:         fmt.Sprintf("sha256:%x", digest[:]),
			CandidateCount: selection.CandidateCount,
			SelectedIDs: append(
				[]string(nil),
				selection.SelectedIDs...,
			),
			Truncated: selection.Truncated,
		}, nil
	}
}
