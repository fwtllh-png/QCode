package result

import (
	"encoding/json"

	adaptercontent "github.com/fwtllh-png/QCode/internal/adapter/content"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type PruneStats struct {
	Results, Bytes, OriginalBytes, RetainedBytes int
}

type PruneWindow struct {
	Active       uint64
	CompactLimit uint64
	Total        uint64
	HardLimit    uint64
}

func PruneSurfaces(
	history *[]provider.Message,
	registry *tool.Registry,
	maxBytes int,
	force bool,
	includeLatest bool,
	measure func([]provider.Message) (PruneWindow, error),
) (PruneStats, PruneWindow, error) {
	names := ToolCallNames(*history)
	latest := latestToolCallIDs(*history)
	var stats PruneStats
	var window PruneWindow
	for messageIndex := range *history {
		message := &(*history)[messageIndex]
		for blockIndex := range message.Blocks {
			block := &message.Blocks[blockIndex]
			if block.Type != provider.ContentToolResult ||
				block.ToolResult == nil {
				continue
			}
			if !includeLatest {
				if _, protected := latest[block.ToolResult.CallID]; protected {
					continue
				}
			}
			name := names[block.ToolResult.CallID]
			if name == "" {
				continue
			}
			var value tool.Result
			if err := json.Unmarshal(
				[]byte(block.ToolResult.Content),
				&value,
			); err != nil {
				original := len(block.ToolResult.Content)
				if !force {
					continue
				}
				// Raw surfaces have no structured projection to shrink, but a
				// silent cut is indistinguishable from an absence: spill the
				// original and replace it with a handle-backed notice.
				pruned, changed := registry.PruneRawSurface(
					name,
					block.ToolResult.Content,
					maxBytes,
				)
				if !changed {
					continue
				}
				block.ToolResult.Content = pruned.Content
				block.ToolResult.Admission = adaptercontent.CloneAdmissionReceipt(
					pruned.Admission,
				)
				stats.Results++
				stats.Bytes += original - len(block.ToolResult.Content)
				window, err = measure(*history)
				if err != nil {
					return PruneStats{}, PruneWindow{}, err
				}
				continue
			}
			projected, changed := registry.PruneResultSurface(
				name,
				value,
				maxBytes,
			)
			if !changed {
				continue
			}
			encoded, err := json.Marshal(tool.ModelResult(name, projected))
			if err != nil || len(encoded) >= len(block.ToolResult.Content) {
				continue
			}
			stats.Results++
			stats.Bytes += len(block.ToolResult.Content) - len(encoded)
			block.ToolResult.Content = string(encoded)
			block.ToolResult.IsError = projected.IsError
			block.ToolResult.Admission = adaptercontent.CloneAdmissionReceipt(
				projected.Admission,
			)
			window, err = measure(*history)
			if err != nil {
				return PruneStats{}, PruneWindow{}, err
			}
			if !force &&
				window.Active <= window.CompactLimit &&
				window.Total <= window.HardLimit {
				return stats, window, nil
			}
		}
	}
	if stats.Results == 0 {
		var err error
		window, err = measure(*history)
		return stats, window, err
	}
	return stats, window, nil
}

// CollapseSurfacesBefore replaces consumed tool results before start with
// handle-backed projections. Messages at and after start keep their raw
// surfaces. Durable journal events are unchanged; only the working history
// projection is rewritten.
func CollapseSurfacesBefore(
	history *[]provider.Message,
	registry *tool.Registry,
	start int,
) PruneStats {
	if history == nil || registry == nil || start <= 0 {
		return PruneStats{}
	}
	names := ToolCallNames(*history)
	var stats PruneStats
	limit := min(start, len(*history))
	for messageIndex := 0; messageIndex < limit; messageIndex++ {
		message := &(*history)[messageIndex]
		for blockIndex := range message.Blocks {
			block := &message.Blocks[blockIndex]
			if block.Type != provider.ContentToolResult ||
				block.ToolResult == nil {
				continue
			}
			name := names[block.ToolResult.CallID]
			if name == "" {
				continue
			}
			var value tool.Result
			if err := json.Unmarshal(
				[]byte(block.ToolResult.Content),
				&value,
			); err != nil {
				continue
			}
			projected, changed := registry.PruneResultSurface(name, value, 1)
			if !changed {
				continue
			}
			encoded, err := json.Marshal(tool.ModelResult(name, projected))
			if err != nil || len(encoded) >= len(block.ToolResult.Content) {
				continue
			}
			stats.Results++
			stats.Bytes += len(block.ToolResult.Content) - len(encoded)
			block.ToolResult.Content = string(encoded)
			block.ToolResult.IsError = projected.IsError
			block.ToolResult.Admission = adaptercontent.CloneAdmissionReceipt(
				projected.Admission,
			)
		}
	}
	return stats
}

func latestToolCallIDs(messages []provider.Message) map[string]struct{} {
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		var result map[string]struct{}
		for _, block := range messages[messageIndex].Blocks {
			if block.Type != provider.ContentToolCall || block.ToolCall == nil {
				continue
			}
			if result == nil {
				result = make(map[string]struct{})
			}
			result[block.ToolCall.ID] = struct{}{}
		}
		if len(result) != 0 {
			return result
		}
	}
	return nil
}

func ToolCallNames(messages []provider.Message) map[string]string {
	type identity struct {
		name  string
		count int
	}
	identities := make(map[string]identity)
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.Type != provider.ContentToolCall || block.ToolCall == nil {
				continue
			}
			call := block.ToolCall
			value := identities[call.ID]
			value.name = call.Name
			value.count++
			identities[call.ID] = value
		}
	}
	names := make(map[string]string)
	for callID, value := range identities {
		if value.count == 1 {
			names[callID] = value.name
		}
	}
	return names
}
