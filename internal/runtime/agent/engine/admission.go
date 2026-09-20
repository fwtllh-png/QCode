package engine

import (
	"encoding/json"
	"fmt"
	"reflect"

	adaptercontent "github.com/fwtllh-png/QCode/internal/adapter/content"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
)

// admitToolResultHistory upgrades legacy or externally restored Tool Results
// before they enter the ContextLedger. Already admitted results are idempotent.
func (e *Engine) admitToolResultHistory(
	messages []provider.Message,
) ([]provider.Message, error) {
	result := cloneMessages(messages)
	limit := e.autoCompactLimit()
	names := toolresult.ToolCallNames(result)
	for messageIndex := range result {
		for blockIndex := range result[messageIndex].Blocks {
			block := &result[messageIndex].Blocks[blockIndex]
			if block.Type != provider.ContentToolResult ||
				block.ToolResult == nil {
				continue
			}
			var value tool.Result
			parsed := json.Unmarshal(
				[]byte(block.ToolResult.Content),
				&value,
			) == nil
			if !parsed {
				value = tool.Result{
					Content: block.ToolResult.Content,
					IsError: block.ToolResult.IsError,
				}
			}
			previous := adaptercontent.CloneAdmissionReceipt(
				block.ToolResult.Admission,
			)
			value.Admission = previous
			name := names[block.ToolResult.CallID]
			value, _ = e.options.Tools.AdmitResultWithin(name, value, limit)
			block.ToolResult.IsError = value.IsError
			block.ToolResult.Admission =
				adaptercontent.CloneAdmissionReceipt(value.Admission)
			// A receipt that already covers the parsed content survives
			// admission unchanged, and deterministic encoding means the
			// block's existing bytes are exactly what re-marshaling would
			// write. Skipping the encode keeps per-step admission linear in
			// new results instead of re-serializing the whole history; the
			// parse above still re-validates the receipt against the content.
			if parsed && previous != nil && value.Admission != nil &&
				reflect.DeepEqual(previous, value.Admission) {
				continue
			}
			encoded, err := json.Marshal(tool.ModelResult(name, value))
			if err != nil {
				return nil, fmt.Errorf(
					"encode admitted tool result %q: %w",
					block.ToolResult.CallID,
					err,
				)
			}
			block.ToolResult.Content = string(encoded)
		}
	}
	return result, nil
}
