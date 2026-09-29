package environment

import "context"

type preparationFactsKey struct{}

// WithPreparationFacts captures binding-time facts independently of execution
// failures. Neither the caller nor a reader can mutate the stored snapshot.
func WithPreparationFacts(ctx context.Context, facts []Fact) context.Context {
	return context.WithValue(ctx, preparationFactsKey{}, append([]Fact(nil), facts...))
}

// PreparationFactsFrom returns a copy of the environment preparation snapshot.
// These facts alone do not establish the cause of a command's failure.
func PreparationFactsFrom(ctx context.Context) []Fact {
	facts, _ := ctx.Value(preparationFactsKey{}).([]Fact)
	return append([]Fact(nil), facts...)
}
