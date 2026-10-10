package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func (e *Engine) completionCandidate(
	call provider.ToolCall,
	result tool.Result,
	batchMutated bool,
	batchSize int,
	mutationRevision uint64,
) turnkernel.CompletionCandidate {
	candidate := turnkernel.NewCompletionCandidate(
		call,
		result,
		batchMutated,
		batchSize,
		nil,
	)
	e.planMu.Lock()
	for _, step := range e.plan.Steps {
		if !step.Done() {
			candidate.PlanOpenSteps++
		}
	}
	e.planMu.Unlock()
	return candidate
}

func bindCompletionDecision(
	result *tool.Result,
	decision turnkernel.CompletionDecision,
) {
	turnkernel.BindCompletionDecision(result, decision)
}
