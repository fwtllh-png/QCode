// Package tooltest provides direct executor helpers for tests that do not
// exercise Tool Guard policy or lifecycle behavior.
package tooltest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func Execute(
	ctx context.Context,
	registry *tool.Registry,
	call tool.Call,
) (tool.Result, error) {
	name, descriptor, executor, err := registry.Resolve(call.Name)
	if err != nil {
		return tool.Result{}, err
	}
	arguments := tool.RepairArguments(json.RawMessage(call.Arguments))
	if err := tool.ValidateArguments(descriptor.InputSchema, arguments); err != nil {
		return tool.Result{}, fmt.Errorf("tool %q arguments: %w", name, err)
	}
	result, _, err := registry.ExecutePreparedOutcome(
		ctx,
		name,
		arguments,
		executor,
	)
	return result, err
}
