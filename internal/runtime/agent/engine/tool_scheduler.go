package engine

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

const defaultMaxToolConcurrent = 8

// toolScheduler adapts the turn execution budget to Tool admission. Resource
// conflicts are serialized separately by Guard Claims.
type toolScheduler struct {
	budget *tool.ExecutionBudget
}

func newToolScheduler(maxConcurrent int) *toolScheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxToolConcurrent
	}
	return &toolScheduler{budget: tool.NewExecutionBudget(maxConcurrent)}
}

func (s *toolScheduler) Admit(
	ctx context.Context,
	_ tool.ParallelPolicy,
) (func(), error) {
	if s == nil || s.budget == nil {
		return func() {}, nil
	}
	return s.budget.Acquire(ctx)
}

func (s *toolScheduler) Active() int  { return s.budget.Active() }
func (s *toolScheduler) Waiting() int { return s.budget.Waiting() }
