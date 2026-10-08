package engine

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// Called only with e.mu held, outside a running sample. Generation uses only
// the leaf mailbox lock, so a foreground turn never joins provider work.
func (e *Engine) installPendingNarrative(ctx context.Context) NarrativeGenerationResult {
	e.narrativeMu.Lock()
	p := e.pendingNarrative
	if p == nil || !p.ready {
		e.narrativeMu.Unlock()
		return NarrativeGenerationResult{}
	}
	e.pendingNarrative = nil
	result := p.candidate
	e.narrativeMu.Unlock()
	defer p.cancel()
	result = e.installNarrative(ctx, p.snapshot, result)
	p.notify(result)
	return result
}

func (e *Engine) installNarrative(ctx context.Context, snapshot narrativeSnapshot, result NarrativeGenerationResult) (installed NarrativeGenerationResult) {
	defer func() { annotateNarrativeReceipt(&installed) }()
	fallback := func(reason string) NarrativeGenerationResult {
		result.Fallback = true
		result.FailureReason = reason
		result.Receipt = narrativeFallback(reason).Receipt
		result.Receipt.CompactionID = snapshot.jobID
		result.Receipt.NarrativeInputTokens = result.Usage.InputTokens
		result.Receipt.NarrativeOutputTokens = result.Usage.OutputTokens
		return result
	}
	e.narrativeMu.Lock()
	closed := e.narrativeClosed
	e.narrativeMu.Unlock()
	if closed || e.stateEpoch != snapshot.epoch {
		return fallback("stale_epoch_or_closed_thread")
	}
	route, err := e.SummaryRouteDigest()
	if err != nil || route != snapshot.input.RouteDigest || narrativePermission(e.options) != snapshot.permission {
		return fallback("route_or_permission_changed")
	}
	if !slices.Equal(snapshot.replacements, narrativeReplacements(e.context.Conversation(), snapshot.input)) {
		return fallback("source_superseded")
	}
	if !result.Fallback {
		if err := result.Artifact.Validate(time.Now().UTC()); err != nil {
			return fallback(err.Error())
		}
	}
	if e.context.Window().ID != snapshot.windowID {
		return fallback("source_window_replaced")
	}
	for _, sourceTurn := range snapshot.sourceTurns {
		if alive := snapshot.options.SessionForTurn; alive != nil {
			owner, exists := alive(ctx, string(sourceTurn))
			if !exists || owner != snapshot.sessionID {
				return fallback("source_turn_unavailable")
			}
		}
		if store := e.options.TurnContexts; store != nil {
			withdrawn, checkErr := store.TurnWithdrawn(ctx, snapshot.threadID, sourceTurn)
			if checkErr != nil || withdrawn {
				return fallback("source_turn_withdrawn_or_unavailable")
			}
		}
	}
	// Compare only captured sources, not the whole authority digest. New Plan
	// progress and unrelated turns can coexist with this representation.
	current, buildErr := agentcontext.BuildNarrativeInput(snapshot.threadID, snapshot.windowID, snapshot.input.AuthorityDigest, route, e.history, snapshot.options.Context.NarrativeLimits, snapshot.input.ExpiresAt, -1)
	if buildErr != nil {
		return fallback(buildErr.Error())
	}
	for _, excerpt := range snapshot.input.Excerpts {
		found := false
		for _, live := range current.Excerpts {
			if live.Source.MessageID == excerpt.Source.MessageID && live.Source.Digest == excerpt.Source.Digest {
				found = true
				break
			}
		}
		if !found && !snapshot.detached {
			return fallback("source_changed_or_removed")
		}
	}
	compaction := e.context.Compaction()
	for _, key := range snapshot.keys {
		if !slices.Contains(compaction.NarrativeAttempts, key) {
			compaction.NarrativeAttempts = append(compaction.NarrativeAttempts, key)
		}
	}
	if !result.Fallback {
		body, renderErr := agentcontext.RenderNarrativeDigest(result.Artifact, 0)
		var sources []provider.Message
		indices := map[string]int{}
		for _, excerpt := range snapshot.input.Excerpts {
			id := excerpt.Source.MessageID
			index, found := indices[id]
			if !found {
				indices[id] = len(sources)
				sources = append(sources, provider.TextMessage(excerpt.Role, excerpt.Text))
			} else {
				sources[index].Blocks[0].Text += excerpt.Text
			}
		}
		before, estimateErr := snapshot.options.TokenEstimator.Estimate(sources)
		after, outputErr := snapshot.options.TokenEstimator.Estimate([]provider.Message{provider.TextMessage(provider.RoleSystem, body)})
		if renderErr != nil || estimateErr != nil || outputErr != nil || after >= before {
			result = fallback("no_net_compression_benefit")
		} else {
			artifact := agentcontext.MergeNarrativeRepresentation(compaction.Digest, result.Artifact)
			compaction.Digest = &artifact
		}
	}
	// Reuse Context's durable CAS/manifest commit. Build from *current* state
	// and replace only Compaction's representation, retaining Plan and focus.
	if store, ok := e.options.TurnContexts.(agentcontext.NarrativeContextStore); ok {
		base := e.sessionRevision
		state, stateErr := e.buildContextSnapshot(e.history, compaction, base+1, e.stateEpoch)
		if stateErr != nil {
			return fallback(stateErr.Error())
		}
		commit := agentcontext.CurrentContextCommit{ID: "narrative:" + snapshot.jobID, ThreadID: snapshot.threadID, TurnID: snapshot.turnID, BaseRevision: &base, Snapshot: state, ManifestLimits: agentcontext.ManifestLimits{OwnerDeltaMaxSegments: e.options.Context.OwnerDeltaMaxSegments, OwnerDeltaMaxBytes: e.options.Context.OwnerDeltaMaxBytes}}
		if commitErr := store.CommitCurrentContext(ctx, commit); commitErr != nil {
			return fallback(fmt.Sprintf("narrative commit: %v", commitErr))
		}
		e.sessionRevision = state.Revision
	}
	e.context.SetCompaction(compaction)
	if result.Fallback {
		return fallback(result.FailureReason)
	}
	result.Receipt = digestReceipt(&result.Artifact, result.Usage, false, "")
	result.Receipt.CompactionID = snapshot.jobID
	result.Receipt.NarrativeProvider = result.Provider
	result.Receipt.NarrativeModel = result.Model
	metadata := result.ModelMetadata
	result.Receipt.NarrativeMetadata = &metadata
	return result
}

func annotateNarrativeReceipt(result *NarrativeGenerationResult) {
	if result.Receipt == nil {
		return
	}
	r := result.Receipt
	r.NarrativeDurationMS = result.DurationMS
	r.NarrativeBackground = result.Background
	r.NarrativeCostKnown = result.CostKnown
	if result.CostUSD > 0 && !math.IsInf(result.CostUSD, 0) && !math.IsNaN(result.CostUSD) {
		r.NarrativeCostMicrounits = uint64(math.Round(result.CostUSD * 1e6))
	}
	r.NarrativeInputTokens, r.NarrativeOutputTokens = result.Usage.InputTokens, result.Usage.OutputTokens
}
