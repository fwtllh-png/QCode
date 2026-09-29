package app

import (
	"context"
	"fmt"
	"strings"

	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	appextension "github.com/fwtllh-png/QCode/internal/runtime/app/extension"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type sessionTitleStore interface {
	UpdateGeneratedTitle(context.Context, string, protocol.ThreadID, uint64,
		protocol.SessionTitleSource, string) (protocol.SessionSummary, bool, error)
}

type sessionTitleEngine interface {
	GenerateSessionTitle(context.Context, protocol.ThreadID, string, string) (agentengine.SessionTitleResult, error)
}

func (r *SessionService) prepareSessionTitle(
	ctx context.Context, summary protocol.SessionSummary,
	operation protocol.Operation, start *protocol.StartTurnPayload,
) {
	store, ok := r.runtime.sessionLifecycle.(sessionTitleStore)
	if !ok || start.ThreadID != summary.ThreadID ||
		!needsSessionTitle(summary.TitleSource) ||
		summary.ParentThreadID != "" {
		return
	}
	prompt := start.DisplayPrompt
	if prompt == "" {
		prompt = start.Prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return
	}
	generator, ok := r.runtime.engine.(sessionTitleEngine)
	if !ok {
		return
	}
	r.titleMu.Lock()
	if r.titleJobs == nil {
		r.titleJobs = make(map[string]sessionTitleJob)
	}
	previous := r.titleJobs[summary.SessionID]
	if previous.running || previous.turnID == start.TurnID {
		r.titleMu.Unlock()
		return
	}
	r.titleJobs[summary.SessionID] = sessionTitleJob{turnID: start.TurnID, running: true}
	r.titleMu.Unlock()
	finish := func() {
		r.titleMu.Lock()
		r.titleJobs[summary.SessionID] = sessionTitleJob{turnID: start.TurnID}
		r.titleMu.Unlock()
	}
	if !r.runtime.OperationService.trackWhileAccepting(&r.titleWorkers) {
		r.titleMu.Lock()
		r.titleJobs[summary.SessionID] = previous
		r.titleMu.Unlock()
		return
	}
	go func() {
		defer r.titleWorkers.Done()
		defer finish()
		current, err := r.runtime.sessionLifecycle.GetLifecycle(r.runtime.ctx, summary.SessionID)
		if err != nil || current.Archived || current.ThreadID != summary.ThreadID ||
			!needsSessionTitle(current.TitleSource) ||
			current.TitleRevision != summary.TitleRevision {
			return
		}
		identity := fmt.Sprintf("%s:%d:%s", current.SessionID, current.TitleRevision, start.TurnID)
		result, err := generator.GenerateSessionTitle(r.runtime.ctx, current.ThreadID, identity, prompt)
		if result.Usage.Total() != 0 {
			_ = r.runtime.publish(operation.ID, start.ThreadID, start.TurnID, start.ItemID, &protocol.UsageData{
				Sample:   contextCompactionSample("session-title:"+identity, 1),
				Provider: result.Provider, Model: result.Model, ModelMetadata: result.ModelMetadata,
				InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
				ReasoningTokens: result.Usage.ReasoningTokens, CachedTokens: result.Usage.CachedTokens,
				CostMicrounits: appextension.CostMicrounits(result.CostUSD), CostKnown: result.CostKnown,
			})
		}
		if err != nil {
			return
		}
		summary, changed, err := store.UpdateGeneratedTitle(r.runtime.ctx,
			current.SessionID, current.ThreadID, current.TitleRevision,
			protocol.SessionTitleAuto, result.Title)
		if err == nil && changed {
			r.publishSessionTitle(operation, summary)
		}
	}()
}

type sessionTitleJob struct {
	turnID  protocol.TurnID
	running bool
}

func needsSessionTitle(source protocol.SessionTitleSource) bool {
	return source == protocol.SessionTitleDefault || source == protocol.SessionTitleTemporary
}

func (r *SessionService) publishSessionTitle(operation protocol.Operation, summary protocol.SessionSummary) {
	threadID, turnID, itemID := protocol.OperationReferences(operation)
	_ = r.runtime.publish(operation.ID, threadID, turnID, itemID, &protocol.SessionTitleUpdatedData{
		SessionID: summary.SessionID, Title: summary.Title,
		TitleSource: summary.TitleSource, TitleRevision: summary.TitleRevision,
	})
}
