package agentcontext

import (
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const recoveryEvidenceMarker = "<recovery_evidence>"
const recoverySourceRequestMarker = "<source_request turn="

// RecoveryBaseHistory projects the history a recovery Turn starts from.
// Retry re-executes the source Turn from its original request, so the whole
// source conversation is removed. Continue carries the source goal forward
// inside the new Turn's envelope: only the stale envelope wrappers are
// deduplicated while the source Turn's closed exchanges stay model-visible,
// because knowing a file was read cannot replace the read itself.
func RecoveryBaseHistory(
	history []provider.Message,
	historyTurns map[string]uint64,
	recovery *protocol.TurnRecoveryContext,
) []provider.Message {
	result := CloneMessages(history)
	if recovery == nil || historyTurns == nil {
		return result
	}
	sourceTurn, ok := historyTurns[string(recovery.SourceTurnID)]
	if !ok || sourceTurn == 0 {
		return result
	}
	continueRecovery := recovery.Action == protocol.TurnRecoveryContinue
	filtered := result[:0]
	for _, message := range result {
		if message.Turn == sourceTurn {
			if continueRecovery && !isRecoveryEnvelopeMessage(message) {
				filtered = append(filtered, message)
			}
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func isRecoveryEnvelopeMessage(message provider.Message) bool {
	if message.Role != provider.RoleUser {
		return false
	}
	text := message.Text()
	return strings.Contains(text, recoveryEvidenceMarker) ||
		strings.Contains(text, recoverySourceRequestMarker)
}

func ReconcileHistoryTurns(
	bindings *map[string]uint64,
	history []provider.Message,
	turnID string,
	turn uint64,
) {
	if *bindings == nil {
		*bindings = make(map[string]uint64)
	}
	visible := make(map[uint64]struct{}, len(history))
	for _, message := range history {
		if message.Turn != 0 {
			visible[message.Turn] = struct{}{}
		}
	}
	reconcileHistoryTurnBindingsFromSet(*bindings, visible)
	if turnID != "" && turn != 0 {
		if _, ok := visible[turn]; ok {
			(*bindings)[turnID] = turn
		}
	}
}

func reconcileHistoryTurnBindingsFromSet(
	bindings map[string]uint64,
	visible map[uint64]struct{},
) {
	for turnID, turn := range bindings {
		if _, ok := visible[turn]; !ok {
			delete(bindings, turnID)
		}
	}
}

func CloneHistoryTurns(source map[string]uint64) map[string]uint64 {
	return maps.Clone(source)
}

func ActiveTurnGoal(history []provider.Message) string {
	if len(history) == 0 {
		return ""
	}
	activeTurn := history[len(history)-1].Turn
	for _, message := range history {
		if message.Turn == activeTurn &&
			message.Role == provider.RoleUser &&
			message.Text() != "" {
			return unwrapRecoveryGoal(message.Text())
		}
	}
	return ""
}

func unwrapRecoveryGoal(text string) string {
	_, body, ok := strings.Cut(text, "<source_request>")
	if !ok {
		return text
	}
	body, _, ok = strings.Cut(body, "</source_request>")
	if !ok {
		return text
	}
	if goal := strings.TrimSpace(body); goal != "" {
		return goal
	}
	return text
}

func RemoveGoalDigest(digest []string, goal string) []string {
	goal = "user: " + strings.Join(strings.Fields(goal), " ")
	for index, line := range digest {
		if line == goal {
			return append(
				append([]string(nil), digest[:index]...),
				digest[index+1:]...,
			)
		}
	}
	return digest
}

func RetainedTailCuts(
	history []provider.Message,
	allowCurrentTurn bool,
	recentTurns int,
	recentMaxTokens uint64,
	estimateTokens func([]provider.Message) uint64,
) []int {
	cuts := compactionCuts(history, allowCurrentTurn)
	if len(cuts) == 0 || recentTurns <= 0 && recentMaxTokens == 0 {
		return cuts
	}
	minimumStart := recentTurnStart(history, recentTurns)
	var preferred, fallback []int
	for _, cut := range cuts {
		if recentMaxTokens != 0 &&
			estimateTokens(history[cut:]) > recentMaxTokens {
			continue
		}
		if minimumStart >= 0 && cut <= minimumStart {
			preferred = append(preferred, cut)
			continue
		}
		fallback = append(fallback, cut)
	}
	if len(preferred) != 0 {
		return preferred
	}
	return fallback
}

func compactionCuts(
	history []provider.Message,
	allowCurrentTurn bool,
) []int {
	if len(history) == 0 {
		return nil
	}
	var cuts []int
	currentTurn := history[len(history)-1].Turn
	for cut := 1; cut < len(history); cut++ {
		if !safeToolBoundary(history, cut) {
			continue
		}
		if !allowCurrentTurn && history[cut-1].Turn == currentTurn {
			continue
		}
		if history[cut-1].Turn != history[cut].Turn || allowCurrentTurn {
			cuts = append(cuts, cut)
		}
	}
	return cuts
}

func recentTurnStart(history []provider.Message, count int) int {
	if count <= 0 {
		return -1
	}
	seen := make(map[uint64]struct{}, count)
	start := -1
	for index := len(history) - 1; index >= 0; index-- {
		turn := history[index].Turn
		if turn == 0 {
			continue
		}
		if _, exists := seen[turn]; !exists {
			if len(seen) == count {
				break
			}
			seen[turn] = struct{}{}
		}
		start = index
	}
	if len(seen) == 0 {
		return -1
	}
	return start
}

func safeToolBoundary(history []provider.Message, cut int) bool {
	if cut <= 0 || cut >= len(history) {
		return false
	}
	return ToolPairsClosed(history[:cut]) && ToolPairsClosed(history[cut:])
}

func ToolPairsClosed(messages []provider.Message) bool {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, message := range messages {
		for _, call := range messageToolCalls(message) {
			calls[call.ID]++
		}
		if id := messageToolResultID(message); id != "" {
			results[id]++
		}
	}
	if len(calls) != len(results) {
		return false
	}
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			return false
		}
	}
	return true
}

type toolPairIdentity struct {
	name           string
	calls, results int
}

func ToolPairIdentityEquivalent(
	before []provider.Message,
	after []provider.Message,
) bool {
	identities := func(messages []provider.Message) map[string]toolPairIdentity {
		result := make(map[string]toolPairIdentity)
		for _, message := range messages {
			for _, call := range messageToolCalls(message) {
				value := result[call.ID]
				value.name = call.Name
				value.calls++
				result[call.ID] = value
			}
			if id := messageToolResultID(message); id != "" {
				value := result[id]
				value.results++
				result[id] = value
			}
		}
		return result
	}
	left, right := identities(before), identities(after)
	if len(left) != len(right) {
		return false
	}
	for id, value := range left {
		if right[id] != value {
			return false
		}
	}
	return true
}

func HistoryBytes(messages []provider.Message) int {
	size := 0
	for _, message := range messages {
		size += messageSize(message)
	}
	return size
}

func UniqueMessageTurns(messages []provider.Message) []uint64 {
	seen := make(map[uint64]struct{})
	var turns []uint64
	for _, message := range messages {
		if message.Turn == 0 {
			continue
		}
		if _, exists := seen[message.Turn]; exists {
			continue
		}
		seen[message.Turn] = struct{}{}
		turns = append(turns, message.Turn)
	}
	return turns
}

func TruncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func TruncateUTF8Tail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
}

// summaryIdentityBytes is the existing digest excerpt for one tool-call
// argument list or tool-result body. The full payload stays on the Handle.
const summaryIdentityBytes = 160

func SummaryOriginalBytes(messages []provider.Message, lineBytes int) int {
	names := toolCallNames(messages)
	total := 0
	for _, message := range messages {
		total += len(SummaryLine(message, lineBytes, names)) + 1
	}
	return total
}

func toolCallNames(messages []provider.Message) map[string]string {
	names := make(map[string]string)
	for _, message := range messages {
		for _, call := range messageToolCalls(message) {
			if call.ID == "" {
				continue
			}
			names[call.ID] = call.Name
		}
	}
	return names
}

func SummaryLine(message provider.Message, lineBytes int, callNames map[string]string) string {
	line := string(message.Role) + ": "
	switch {
	case message.Text() != "":
		line += strings.Join(strings.Fields(message.Text()), " ")
	case len(messageToolCalls(message)) != 0:
		calls := make([]string, 0, len(messageToolCalls(message)))
		for _, call := range messageToolCalls(message) {
			arguments := compactSummaryShape(call.Arguments)
			calls = append(calls, strings.TrimSpace(call.ID+" "+call.Name+" "+arguments))
		}
		line += "tool calls " + strings.Join(calls, ", ")
	default:
		if result := messageToolResult(message); result != nil {
			line += toolResultSummaryLine(*result, callNames)
			break
		}
		line += strings.Join(strings.Fields(blocksReasoning(message.Blocks)), " ")
	}
	if lineBytes > 0 && len(line) > lineBytes {
		line = TruncateUTF8(line, lineBytes) + "..."
	}
	return line
}

func toolResultSummaryLine(result provider.ToolResult, callNames map[string]string) string {
	parts := []string{"tool result", result.CallID}
	if name := strings.TrimSpace(callNames[result.CallID]); name != "" {
		parts = append(parts, name)
	}
	if result.IsError {
		parts = append(parts, "error")
	}
	if shape := compactSummaryShape(result.Content); shape != "" {
		parts = append(parts, shape)
	}
	return strings.Join(parts, " ")
}

func compactSummaryShape(value string) string {
	shape := strings.Join(strings.Fields(value), " ")
	if len(shape) > summaryIdentityBytes {
		return TruncateUTF8(shape, summaryIdentityBytes) + "..."
	}
	return shape
}

func messageSize(message provider.Message) int {
	size := 0
	if message.Provenance != nil {
		size += len(message.Provenance.Adapter) +
			len(message.Provenance.Provider) +
			len(message.Provenance.Model)
		if message.Provenance.Replay != nil {
			size += len(message.Provenance.Replay.ContentDigest) +
				len(message.Provenance.Replay.Data)
		}
	}
	for _, block := range message.Blocks {
		size += len(block.Text)
		if block.ToolCall != nil {
			size += len(block.ToolCall.ID) +
				len(block.ToolCall.Name) +
				len(block.ToolCall.Arguments)
		}
		if block.ToolResult != nil {
			size += len(block.ToolResult.CallID) +
				len(block.ToolResult.Content)
		}
	}
	return size
}

func messageToolCalls(message provider.Message) []provider.ToolCall {
	var calls []provider.ToolCall
	for _, block := range message.Blocks {
		if block.ToolCall != nil {
			calls = append(calls, *block.ToolCall)
		}
	}
	return calls
}

func messageToolResult(message provider.Message) *provider.ToolResult {
	for _, block := range message.Blocks {
		if block.ToolResult != nil {
			return block.ToolResult
		}
	}
	return nil
}

func messageToolResultID(message provider.Message) string {
	if result := messageToolResult(message); result != nil {
		return result.CallID
	}
	return ""
}

func blocksReasoning(blocks []provider.ContentBlock) string {
	var values []string
	for _, block := range blocks {
		if block.Type == provider.ContentReasoning {
			values = append(values, block.Text)
		}
	}
	return strings.Join(values, "\n")
}
