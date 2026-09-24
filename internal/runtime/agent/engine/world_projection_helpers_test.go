package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func blocksText(blocks []provider.ContentBlock) string {
	return providerassembly.BlocksText(blocks)
}

func messageToolCalls(message provider.Message) []provider.ToolCall {
	return providerassembly.MessageToolCalls(message)
}

func messageToolResultID(message provider.Message) string {
	return providerassembly.MessageToolResultID(message)
}

func projectionRecoveryID(
	recovery *protocol.TurnRecoveryContext,
) string {
	return providerassembly.ProjectionRecoveryID(recovery)
}

func estimateCost(pricing model.Pricing, usage provider.Usage) float64 {
	return provider.EstimateCost(pricing, usage)
}

func estimateMessageTokens(messages []provider.Message) uint64 {
	return agentcontext.EstimateMessageTokens(messages)
}

func pricingKnown(pricing model.Pricing, usage provider.Usage) bool {
	return provider.PricingKnown(pricing, usage)
}

func (e *Engine) observePath(
	source agentcontext.WorkingSetSource,
	path string,
) {
	e.contextAuthority().ObservePath(
		e.options.Workspace,
		source,
		e.turn,
		path,
	)
}

func (e *Engine) observePaths(
	source agentcontext.WorkingSetSource,
	paths []string,
) {
	for _, path := range paths {
		e.observePath(source, path)
	}
}

func (e *Engine) noteToolCall(call provider.ToolCall) {
	e.contextAuthority().NoteToolCall(call)
}

func (e *Engine) observeEvidence(
	call provider.ToolCall,
	result tool.Result,
) {
	e.contextAuthority().ObserveToolResult(
		e.options.Workspace,
		call,
		result,
		e.turn,
	)
}

func (e *Engine) observeChangeEvidence(change tool.WorkspaceChange) {
	e.contextAuthority().ObserveChange(
		e.options.Workspace,
		change,
		e.turn,
	)
}

func (e *Engine) observeDiagnosticsEvidence(
	receipts []diagnostics.Receipt,
) {
	e.contextAuthority().ObserveDiagnostics(
		e.options.Workspace,
		receipts,
	)
}

func (e *Engine) observeVerifiedEvidence(paths []string) {
	e.contextAuthority().ObserveVerified(e.options.Workspace, paths)
}

func (e *Engine) frozenWorldSections(
	spec TurnSpec,
	turn uint64,
) ([]agentcontext.WorldSection, []promptcontext.Receipt) {
	var sections []agentcontext.WorldSection
	var receipts []promptcontext.Receipt
	appendSection := func(id, source, body, digest string) {
		messages, receipt := promptcontext.AssembleWorldText(
			id, source, body, e.contextBudget(id),
		)
		if digest != "" {
			receipt.Digest = digest
		}
		sections = append(sections, promptcontext.WorldSectionFromReceipt(
			receipt,
			promptcontext.FirstMessage(messages),
			turn,
		))
		receipts = append(receipts, receipt)
	}
	appendSection(
		promptcontext.PartitionMode,
		"session://profile.mode",
		promptcontext.ActInstructionPack(),
		"",
	)
	policy := promptcontext.NewPolicySection(spec.Policy)
	appendSection(
		policy.ID(),
		"worldstate://"+policy.ID(),
		policy.Render(),
		policy.Digest(),
	)
	if e.options.CodingPolicy {
		coding := promptcontext.NewCodingPolicySection()
		appendSection(
			coding.ID(),
			"worldstate://"+coding.ID(),
			coding.Render(),
			coding.Digest(),
		)
	}
	if section, receipt, ok := e.memoryWorldSection(spec, turn); ok {
		if section.ID != "" {
			sections = append(sections, section)
		}
		receipts = append(receipts, receipt)
	}
	if body := promptcontext.RenderSkillWorld(spec.Skills); body != "" {
		appendSection(
			promptcontext.PartitionSkills,
			"skill://catalog",
			body,
			"",
		)
	}
	return sections, receipts
}

func (e *Engine) memoryWorldSection(
	spec TurnSpec,
	turn uint64,
) (agentcontext.WorldSection, promptcontext.Receipt, bool) {
	return promptcontext.MemoryWorldSection(
		spec.Memory,
		e.contextBudget(promptcontext.PartitionUserMemory),
		turn,
	)
}

func (e *Engine) contextBudget(kind string) promptcontext.Budget {
	if budget, ok := e.options.ContextBudgets[kind]; ok {
		return budget
	}
	return promptcontext.Budget{}
}
