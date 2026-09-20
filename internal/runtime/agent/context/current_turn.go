package agentcontext

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// PriorDecisionProjectionPrefix marks a pressure-path rewrite of closed-round
// assistant commentary. The line is a sourced projection, not authority.
const PriorDecisionProjectionPrefix = "(non-authoritative prior decision) "

// CurrentTurnUserIndex is the first non-world user message of the active
// turn. The sampling path pins this request when collapsing closed groups.
func CurrentTurnUserIndex(history []provider.Message) int {
	current := lastNonWorldTurn(history)
	if current == 0 {
		return -1
	}
	for index, message := range history {
		if IsWorldStateMessage(message) {
			continue
		}
		if message.Turn == current && message.Role == provider.RoleUser {
			return index
		}
	}
	return -1
}

// CurrentTurnWorkingSetCuts returns safe cuts after the pinned user request
// that excise at least one closed tool pair. Prefix folds that would hide the
// user are not used; the caller keeps the user message on the retained side.
func CurrentTurnWorkingSetCuts(history []provider.Message) []int {
	user := CurrentTurnUserIndex(history)
	if user < 0 {
		return nil
	}
	var cuts []int
	for _, cut := range compactionCuts(history, true) {
		if cut <= user+1 {
			continue
		}
		excised := StripWorldState(history[user+1 : cut])
		if !currentTurnExcisionHasClosedPair(excised) {
			continue
		}
		cuts = append(cuts, cut)
	}
	return cuts
}

func currentTurnExcisionHasClosedPair(messages []provider.Message) bool {
	if len(messages) == 0 || !ToolPairsClosed(messages) {
		return false
	}
	for _, message := range messages {
		if len(messageToolCalls(message)) != 0 || messageToolResultID(message) != "" {
			return true
		}
	}
	return false
}

// LatestCurrentTurnWorld keeps the newest current-turn world message for each
// section and drops earlier append-only patches of the same turn.
func LatestCurrentTurnWorld(history []provider.Message) []provider.Message {
	current := lastNonWorldTurn(history)
	if current == 0 {
		return nil
	}
	latest := map[string]int{}
	for index, message := range history {
		entry, _, ok := InspectWorldMessage(message)
		if !ok || message.Turn != current {
			continue
		}
		latest[entry.ID] = index
	}
	if len(latest) == 0 {
		return nil
	}
	kept := make([]provider.Message, 0, len(latest))
	for index, message := range history {
		entry, _, ok := InspectWorldMessage(message)
		if !ok || message.Turn != current || latest[entry.ID] != index {
			continue
		}
		kept = append(kept, CloneMessage(message))
	}
	return kept
}

// CollapseCurrentTurnWorld drops superseded current-turn world patches.
func CollapseCurrentTurnWorld(history []provider.Message) ([]provider.Message, bool) {
	current := lastNonWorldTurn(history)
	if current == 0 {
		return history, false
	}
	latest := map[string]int{}
	for index, message := range history {
		entry, _, ok := InspectWorldMessage(message)
		if !ok || message.Turn != current {
			continue
		}
		latest[entry.ID] = index
	}
	if len(latest) == 0 {
		return history, false
	}
	kept := make([]provider.Message, 0, len(history))
	changed := false
	for index, message := range history {
		entry, _, ok := InspectWorldMessage(message)
		if ok && message.Turn == current && latest[entry.ID] != index {
			changed = true
			continue
		}
		kept = append(kept, message)
	}
	if !changed {
		return history, false
	}
	return kept, true
}

// StripConsumedReasoning removes reasoning from every assistant message except
// the latest. keepLatest=false also drops the newest assistant reasoning.
func StripConsumedReasoning(history []provider.Message, keepLatest bool) int {
	lastAssistant := -1
	for index, message := range history {
		if IsWorldStateMessage(message) || message.Role != provider.RoleAssistant {
			continue
		}
		lastAssistant = index
	}
	removed := 0
	for index := range history {
		if keepLatest && index == lastAssistant {
			continue
		}
		blocks := make([]provider.ContentBlock, 0, len(history[index].Blocks))
		changed := false
		for _, block := range history[index].Blocks {
			if block.Type == provider.ContentReasoning {
				removed++
				changed = true
				continue
			}
			blocks = append(blocks, block)
		}
		if !changed {
			continue
		}
		if len(blocks) == 0 {
			blocks = []provider.ContentBlock{{Type: provider.ContentText}}
		}
		history[index].Blocks = blocks
	}
	return removed
}

// SummarizeClosedAssistantText rewrites closed-round ContentText into a sourced
// non-authoritative line. Latest-batch commentary stays intact unless
// includeLatest is true. Short text that already fits lineBytes is unchanged.
func SummarizeClosedAssistantText(
	history []provider.Message,
	includeLatest bool,
	lineBytes int,
) int {
	if lineBytes <= 0 {
		return 0
	}
	latest := latestHistoryToolCallIDs(history)
	results := historyToolResultIDs(history)
	changed := 0
	for index := range history {
		message := &history[index]
		if message.Role != provider.RoleAssistant ||
			!containsClosedToolCall(message.Blocks, results) {
			continue
		}
		if !includeLatest && messageHasToolCallID(*message, latest) {
			continue
		}
		summary := ClosedAssistantTextSummary(*message, lineBytes)
		if summary == "" || !closedAssistantTextNeedsSummary(*message, summary) {
			continue
		}
		message.Blocks = replaceAssistantTextBlocks(message.Blocks, summary)
		changed++
	}
	return changed
}

func ClosedAssistantTextSummary(message provider.Message, lineBytes int) string {
	text := strings.Join(strings.Fields(message.Text()), " ")
	if text == "" {
		return ""
	}
	var sources []string
	for _, call := range messageToolCalls(message) {
		if call.ID != "" {
			sources = append(sources, call.ID)
		}
	}
	line := PriorDecisionProjectionPrefix
	if len(sources) != 0 {
		line += "source=" + strings.Join(sources, ",") + " "
	}
	line += text
	if len(line) > lineBytes {
		line = TruncateUTF8(line, lineBytes) + "..."
	}
	return line
}

func closedAssistantTextNeedsSummary(message provider.Message, summary string) bool {
	original := 0
	blocks := 0
	for _, block := range message.Blocks {
		if block.Type != provider.ContentText {
			continue
		}
		blocks++
		original += len(block.Text)
	}
	return blocks > 1 || len(summary) < original
}

func replaceAssistantTextBlocks(
	blocks []provider.ContentBlock,
	summary string,
) []provider.ContentBlock {
	replaced := false
	result := make([]provider.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != provider.ContentText {
			result = append(result, block)
			continue
		}
		if replaced {
			continue
		}
		block.Text = summary
		result = append(result, block)
		replaced = true
	}
	return result
}

func containsClosedToolCall(
	blocks []provider.ContentBlock,
	results map[string]struct{},
) bool {
	for _, block := range blocks {
		if block.ToolCall == nil {
			continue
		}
		if _, found := results[block.ToolCall.ID]; found {
			return true
		}
	}
	return false
}

func messageHasToolCallID(message provider.Message, ids map[string]struct{}) bool {
	if len(ids) == 0 {
		return false
	}
	for _, call := range messageToolCalls(message) {
		if _, found := ids[call.ID]; found {
			return true
		}
	}
	return false
}

func historyToolResultIDs(history []provider.Message) map[string]struct{} {
	results := make(map[string]struct{})
	for _, message := range history {
		if id := messageToolResultID(message); id != "" {
			results[id] = struct{}{}
		}
	}
	return results
}

// BoundToolCallArguments replaces tool-call bodies with identity-only JSON.
// Latest-batch calls stay intact unless includeLatest is true. keysByName is
// the catalog-resolved identity map; a missing name uses the default
// whitelist.
func BoundToolCallArguments(
	history []provider.Message,
	includeLatest bool,
	keysByName map[string][]string,
) int {
	latest := latestHistoryToolCallIDs(history)
	changed := 0
	for index := range history {
		for blockIndex := range history[index].Blocks {
			block := &history[index].Blocks[blockIndex]
			if block.Type != provider.ContentToolCall || block.ToolCall == nil {
				continue
			}
			if !includeLatest {
				if _, protected := latest[block.ToolCall.ID]; protected {
					continue
				}
			}
			compacted := ToolCallIdentityArguments(
				block.ToolCall.Arguments,
				keysByName[block.ToolCall.Name],
			)
			if compacted == block.ToolCall.Arguments ||
				len(compacted) >= len(block.ToolCall.Arguments) {
				continue
			}
			block.ToolCall.Arguments = compacted
			changed++
		}
	}
	return changed
}

// ToolCallIdentityArguments keeps only the supplied identity fields. An empty
// key list uses the default public whitelist so undeclared tools still drop
// large bodies.
func ToolCallIdentityArguments(arguments string, keys []string) string {
	return tool.CompactIdentityArguments(arguments, keys, summaryIdentityBytes)
}

func latestHistoryToolCallIDs(history []provider.Message) map[string]struct{} {
	for index := len(history) - 1; index >= 0; index-- {
		calls := messageToolCalls(history[index])
		if len(calls) == 0 {
			continue
		}
		ids := make(map[string]struct{}, len(calls))
		for _, call := range calls {
			if call.ID != "" {
				ids[call.ID] = struct{}{}
			}
		}
		if len(ids) != 0 {
			return ids
		}
	}
	return nil
}
