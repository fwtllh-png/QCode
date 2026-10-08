package turnhistory

import "context"

type lookupKey struct{}

type runtimeLookup struct {
	turn       Lookup
	references ReferenceLookup
}

// WithLookup scopes history recovery to the executing engine even when a
// session fork shares the registered catalog with its parent.
func WithLookup(ctx context.Context, turn Lookup, references ReferenceLookup) context.Context {
	return context.WithValue(ctx, lookupKey{}, runtimeLookup{turn: turn, references: references})
}
