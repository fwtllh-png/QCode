package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func (e *Engine) buildTruthCapsule(
	summary agentcontext.Summary,
	history []provider.Message,
) agentcontext.TruthCapsule {
	route := e.activeRoute()
	model := route.Model()
	maxDigest := e.options.MaxDigestEntries
	if maxDigest <= 0 {
		maxDigest = defaultMaxDigestEntries
	}
	compatibility := agentcontext.Compatibility{
		SchemaVersion: agentcontext.TruthSchemaVersion,
		Adapter:       string(route.Adapter()), Provider: route.ProviderID(),
		Model: model.ID, ContextTokens: model.Limits.ContextTokens,
		ToolCalls:       model.Capabilities.ToolCalls,
		Reasoning:       model.Capabilities.Reasoning,
		ImageInput:      model.Capabilities.ImageInput || model.Capabilities.Vision,
		SummaryMaxBytes: e.summaryBudget(), MaxDigestEntries: maxDigest,
		DownshiftPolicy: agentcontext.DownshiftRuntimeTruthOnly,
	}
	e.planMu.Lock()
	plan := e.plan.Clone()
	e.planMu.Unlock()
	evidenceDelta := e.evidenceSet().RetainedDelta(
		e.options.Context.TruthRetention.FactMaxEntities,
		e.options.Context.TruthRetention.VerifiedChangeRetentionTurns,
		e.options.Context.TruthRetention.HandleMaxEntities,
	)
	workspaceDigests := make(map[string]string)
	if binding, err := e.captureWorkspaceBindingFor(evidenceDelta); err == nil {
		for _, path := range binding.BoundPaths {
			workspaceDigests[path.Path] = path.ContentDigest
		}
	}
	return agentcontext.BuildTruthCapsule(agentcontext.TruthProjection{
		Compatibility: compatibility, ModelID: model.ID,
		ContextTokens: model.Limits.ContextTokens,
		Summary:       summary, Plan: plan, Turn: e.turn,
		Evidence: evidenceDelta, WorkspaceDigests: workspaceDigests,
		CriticalPaths: summary.CriticalPaths,
		ExtraEntities: append(
			append(
				append(
					e.pendingInputTruthEntities(),
					e.continuityTruthEntities()...,
				),
				e.resumeTruthEntities()...,
			),
			e.omittedTurnTruthEntities(history)...,
		),
	})
}
