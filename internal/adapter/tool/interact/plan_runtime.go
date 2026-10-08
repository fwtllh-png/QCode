package interact

import "context"

// PlanRuntime binds a shared tool catalog to the engine owning this invocation.
// A registered fallback remains available to standalone tool callers.
type PlanRuntime struct {
	Apply        func(Plan) error
	CurrentSteps func() []PlanStep
}

type planRuntimeKey struct{}

func WithPlanRuntime(ctx context.Context, runtime PlanRuntime) context.Context {
	return context.WithValue(ctx, planRuntimeKey{}, runtime)
}

func planRuntimeFrom(ctx context.Context) (PlanRuntime, bool) {
	runtime, ok := ctx.Value(planRuntimeKey{}).(PlanRuntime)
	return runtime, ok
}
