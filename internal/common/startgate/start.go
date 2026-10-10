// Package startgate carries trusted admission checks to the physical process
// start boundary, after sandbox and descriptor preparation have finished.
package startgate

import "context"

type Gate func(start func() error) error
type key struct{}

func With(ctx context.Context, gate Gate) context.Context {
	return context.WithValue(ctx, key{}, gate)
}

// Run must wrap only process creation, never waiting for process completion.
func Run(ctx context.Context, start func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gate, ok := ctx.Value(key{}).(Gate); ok {
		return gate(start)
	}
	return start()
}
