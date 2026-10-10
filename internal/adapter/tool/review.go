package tool

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ReviewPlan is supplied by a trusted command executor from frozen arguments
// and its prepared environment. Environment values stay in the executor.
type ReviewPlan struct {
	Command, WorkingDir, SourceRoot, EnvironmentDigest, Settlement string
	WritePaths                                                     []string
	Backend                                                        sandbox.Backend
}

type ReviewPlanProvider interface {
	GuardianReviewPlan(PreparedInvocation) (ReviewPlan, error)
}

// ReviewedExecution owns an existing private copy; Isolator.Begin consumes it
// once instead of recopying the mutable workspace. This is not an approval.
type ReviewedExecution interface {
	Isolator
	ValidateInvocation(context.Context, PreparedInvocation) error
}

type reviewedExecutionKey struct{}

func WithReviewedExecution(ctx context.Context, execution ReviewedExecution) context.Context {
	return context.WithValue(ctx, reviewedExecutionKey{}, execution)
}

func ReviewedExecutionFrom(ctx context.Context) ReviewedExecution {
	value, _ := ctx.Value(reviewedExecutionKey{}).(ReviewedExecution)
	return value
}
