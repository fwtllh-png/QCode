package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	contextview "github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) recordToolSurfaceBudget(
	scope *Scope,
	context protocol.SampleContextData,
	admission contextview.EconomicAdmission,
) {
	if scope == nil || e.options.Tools == nil {
		return
	}
	maxBytes, itemBytes := contextview.ToolSurfaceBudget(
		context, admission,
		e.options.Tools.ResultTokenCapacity(),
	)
	scope.mu.Lock()
	scope.state.toolSurfaceMaxBytes = max(1, maxBytes)
	scope.state.toolSurfaceItemBytes = max(1, itemBytes)
	scope.mu.Unlock()
}

type toolResultPruneStats struct {
	results int
	bytes   int
}

func (e *Engine) pruneToolResultSurfaces(
	history *[]provider.Message,
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	force bool,
	includeLatest bool,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
) (toolResultPruneStats, tokenWindow, error) {
	measured, err := e.measureTokenWindow(
		input.WithHistory(agentcontext.ProjectHistory(*history, projectHistory)),
		outputReserve,
		economicInput,
	)
	if err != nil {
		return toolResultPruneStats{}, tokenWindow{}, err
	}
	surfaceBytes := e.dynamicToolResultSurfaceBytes(*history, measured)
	if surfaceBytes == 0 {
		return toolResultPruneStats{}, measured, nil
	}
	stats, window, err := toolresult.PruneSurfaces(
		history,
		e.options.Tools,
		surfaceBytes,
		force,
		includeLatest,
		func(history []provider.Message) (toolresult.PruneWindow, error) {
			measured, err := e.measureTokenWindow(
				input.WithHistory(agentcontext.ProjectHistory(history, projectHistory)),
				outputReserve,
				economicInput,
			)
			return toolresult.PruneWindow{
				Active: measured.active, CompactLimit: measured.compactLimit,
				Total: measured.total, HardLimit: measured.hardLimit,
			}, err
		},
	)
	return toolResultPruneStats{
			results: stats.Results,
			bytes:   stats.Bytes,
		}, tokenWindow{
			active: window.Active, compactLimit: window.CompactLimit,
			total: window.Total, hardLimit: window.HardLimit,
		}, err
}

func (e *Engine) dynamicToolResultSurfaceBytes(
	history []provider.Message,
	window tokenWindow,
) int {
	if window.active <= window.compactLimit &&
		window.total <= window.hardLimit {
		if scope := e.runningScope(); scope != nil {
			scope.mu.Lock()
			itemBytes := scope.state.toolSurfaceItemBytes
			scope.mu.Unlock()
			if itemBytes > 0 {
				return itemBytes
			}
		}
		return 0
	}
	var resultCount uint64
	resultTokens := uint64(0)
	resultBytes := uint64(0)
	for _, message := range history {
		for _, block := range message.Blocks {
			if block.Type == provider.ContentToolResult &&
				block.ToolResult != nil {
				resultTokens += tokenestimate.Text(block.ToolResult.Content)
				resultBytes += uint64(len(block.ToolResult.Content))
				resultCount++
			}
		}
	}
	if resultCount == 0 {
		return 0
	}
	baseTokens := window.active - min(window.active, resultTokens)
	availableTokens := window.compactLimit - min(
		window.compactLimit,
		baseTokens,
	)
	// Convert the spare token budget into per-result bytes at the density the
	// retained results actually have, so a CJK-heavy surface is budgeted at
	// its real bytes-per-token instead of the ASCII ratio.
	bytesPerToken := uint64(4)
	if resultTokens != 0 {
		bytesPerToken = min(uint64(4), max(uint64(1), resultBytes/resultTokens))
	}
	maxInt := uint64(^uint(0) >> 1)
	allowance := maxInt
	if availableTokens <= maxInt/bytesPerToken {
		allowance = availableTokens * bytesPerToken
	}
	bytes := min(maxInt, allowance/resultCount)
	if bytes == 0 {
		return 1
	}
	return int(bytes)
}
