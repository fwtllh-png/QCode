// Package agentcontext owns the typed, immutable projection used for one model
// sample. Durable world-state baselines are added in later CE stages.
package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	adaptercontent "github.com/fwtllh-png/QCode/internal/adapter/content"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type MessageKind string

const (
	KindStable       MessageKind = "stable"
	KindHistory      MessageKind = "history"
	KindDynamic      MessageKind = "dynamic"
	KindContinuation MessageKind = "continuation"
)

var orderedKinds = [...]MessageKind{
	KindStable,
	KindHistory,
	KindDynamic,
	KindContinuation,
}

// MessageItem is one typed model-visible message in a MessageSnapshot.
type MessageItem struct {
	ID      string           `json:"id"`
	Kind    MessageKind      `json:"kind"`
	Role    provider.Role    `json:"role"`
	Message provider.Message `json:"message"`
}

// LedgerInput constructs the single model-context authority at Scope admission.
type LedgerInput struct {
	Stable       []provider.Message
	History      []provider.Message
	Dynamic      []provider.Message
	Continuation []provider.Message
	Definitions  []provider.ToolDefinition
}

// LedgerProjection atomically updates the mutable portions of a MessageLedger.
type LedgerProjection struct {
	Stable       []provider.Message
	History      []provider.Message
	Dynamic      []provider.Message
	Continuation []provider.Message
	Definitions  []provider.ToolDefinition
}

// MessageLedger is the sole owner of model-sample assembly within one turn.
type MessageLedger struct {
	revision    uint64
	partitions  map[MessageKind][]provider.Message
	definitions []provider.ToolDefinition
}

// MessageSnapshot is an immutable model-sample projection.
type MessageSnapshot struct {
	revision    uint64
	partitions  map[MessageKind][]provider.Message
	definitions []provider.ToolDefinition
	items       []MessageItem
}

func NewMessageLedger(input LedgerInput) *MessageLedger {
	return &MessageLedger{
		revision: 1,
		partitions: map[MessageKind][]provider.Message{
			KindStable:       CloneMessages(input.Stable),
			KindHistory:      CloneMessages(input.History),
			KindDynamic:      CloneMessages(input.Dynamic),
			KindContinuation: CloneMessages(input.Continuation),
		},
		definitions: cloneDefinitions(input.Definitions),
	}
}

// Project replaces all mutable partitions as one revision.
func (l *MessageLedger) Project(value LedgerProjection) MessageSnapshot {
	changed := l.replace(KindStable, value.Stable)
	changed = l.replace(KindHistory, value.History) || changed
	changed = l.replace(KindDynamic, value.Dynamic) || changed
	changed = l.replace(KindContinuation, value.Continuation) || changed
	projectedDefinitions := cloneDefinitions(value.Definitions)
	if !reflect.DeepEqual(l.definitions, projectedDefinitions) {
		l.definitions = projectedDefinitions
		changed = true
	}
	if changed {
		l.revision++
	}
	return l.Snapshot()
}

func (l *MessageLedger) Snapshot() MessageSnapshot {
	partitions := make(map[MessageKind][]provider.Message, len(orderedKinds))
	var items []MessageItem
	for _, kind := range orderedKinds {
		partitions[kind] = CloneMessages(l.partitions[kind])
		occurrences := make(map[string]int)
		// Items reference the partition copies — the snapshot is immutable,
		// so cloning each message twice would only double the allocation.
		for _, message := range partitions[kind] {
			id := itemID(kind, message)
			occurrence := occurrences[id]
			occurrences[id]++
			if occurrence != 0 {
				id = fmt.Sprintf("%s_%d", id, occurrence)
			}
			items = append(items, MessageItem{
				ID: id, Kind: kind,
				Role: message.Role, Message: message,
			})
		}
	}
	return MessageSnapshot{
		revision: l.revision, partitions: partitions,
		definitions: cloneDefinitions(l.definitions), items: items,
	}
}

func (s MessageSnapshot) Revision() uint64 {
	return s.revision
}

func (s MessageSnapshot) Items() []MessageItem {
	result := make([]MessageItem, len(s.items))
	for index, item := range s.items {
		result[index] = item
		result[index].Message = CloneMessage(item.Message)
	}
	return result
}

// ItemRefs identifies the snapshot's items in order without cloning their
// messages; prefix manifests and other token accounting that already hold the
// estimates use it to align with MeasureDetailed's per-item results.
func (s MessageSnapshot) ItemRefs() []ItemRef {
	refs := make([]ItemRef, len(s.items))
	for index, item := range s.items {
		refs[index] = ItemRef{ID: item.ID, Kind: item.Kind}
	}
	return refs
}

type ItemRef struct {
	ID   string
	Kind MessageKind
}

func (s MessageSnapshot) Partition(kind MessageKind) []provider.Message {
	return CloneMessages(s.partitions[kind])
}

func (s MessageSnapshot) Definitions() []provider.ToolDefinition {
	return cloneDefinitions(s.definitions)
}

func (s MessageSnapshot) Messages() []provider.Message {
	var result []provider.Message
	for _, kind := range orderedKinds {
		result = append(result, CloneMessages(s.partitions[kind])...)
	}
	return result
}

// Digest identifies the complete model-visible message and definition content.
func (s MessageSnapshot) Digest() (string, error) {
	// Marshal reads the internal partitions directly; cloning every message
	// first would only add allocations to an already O(context) pass. The
	// slice stays nil for an empty snapshot so the encoding is unchanged.
	var messages []provider.Message
	for _, kind := range orderedKinds {
		messages = append(messages, s.partitions[kind]...)
	}
	encoded, err := json.Marshal(struct {
		Messages    []provider.Message        `json:"messages"`
		Definitions []provider.ToolDefinition `json:"definitions,omitempty"`
	}{
		Messages: messages, Definitions: s.definitions,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// WithHistory returns an ephemeral rewrite used while measuring compaction.
func (s MessageSnapshot) WithHistory(history []provider.Message) MessageSnapshot {
	if reflect.DeepEqual(s.partitions[KindHistory], history) {
		return s
	}
	partitions := make(map[MessageKind][]provider.Message, len(s.partitions))
	for _, kind := range orderedKinds {
		partitions[kind] = CloneMessages(s.partitions[kind])
	}
	partitions[KindHistory] = CloneMessages(history)
	ledger := &MessageLedger{
		revision: s.revision + 1, partitions: partitions,
		definitions: cloneDefinitions(s.definitions),
	}
	return ledger.Snapshot()
}

func (l *MessageLedger) ReplaceHistory(history []provider.Message) {
	if l != nil && l.replace(KindHistory, history) {
		l.revision++
	}
}

func (l *MessageLedger) replace(kind MessageKind, messages []provider.Message) bool {
	if reflect.DeepEqual(l.partitions[kind], messages) {
		return false
	}
	l.partitions[kind] = CloneMessages(messages)
	return true
}

func itemID(kind MessageKind, message provider.Message) string {
	encoded, _ := json.Marshal(struct {
		Kind    MessageKind      `json:"kind"`
		Turn    uint64           `json:"turn"`
		Message provider.Message `json:"message"`
	}{
		Kind: kind, Turn: message.Turn, Message: message,
	})
	sum := sha256.Sum256(encoded)
	return "ctx_" + hex.EncodeToString(sum[:])
}

// CloneMessages isolates mutable nested provider content.
func CloneMessages(messages []provider.Message) []provider.Message {
	if messages == nil {
		return nil
	}
	result := make([]provider.Message, len(messages))
	for index, message := range messages {
		result[index] = CloneMessage(message)
	}
	return result
}

// CloneMessage isolates one provider message.
func CloneMessage(message provider.Message) provider.Message {
	result := message
	result.Blocks = CloneBlocks(message.Blocks)
	if message.Provenance != nil {
		value := *message.Provenance
		if message.Provenance.Replay != nil {
			replay := *message.Provenance.Replay
			replay.Data = append([]byte(nil), message.Provenance.Replay.Data...)
			value.Replay = &replay
		}
		result.Provenance = &value
	}
	return result
}

// CloneBlocks isolates nested block payloads, including attachment bytes.
func CloneBlocks(blocks []provider.ContentBlock) []provider.ContentBlock {
	if blocks == nil {
		return nil
	}
	result := make([]provider.ContentBlock, len(blocks))
	for index, block := range blocks {
		result[index] = block
		if block.ToolCall != nil {
			value := *block.ToolCall
			result[index].ToolCall = &value
		}
		if block.ToolResult != nil {
			value := *block.ToolResult
			value.Admission = adaptercontent.CloneAdmissionReceipt(
				block.ToolResult.Admission,
			)
			result[index].ToolResult = &value
		}
		if block.Search != nil {
			value := *block.Search
			value.Sources = append([]provider.Source(nil), block.Search.Sources...)
			result[index].Search = &value
		}
		if block.Citation != nil {
			value := *block.Citation
			result[index].Citation = &value
		}
		if block.Attachment != nil {
			value := *block.Attachment
			value.Data = append([]byte(nil), block.Attachment.Data...)
			result[index].Attachment = &value
		}
	}
	return result
}

// cloneDefinitions deep-clones tool definitions into a canonical order.
// Definitions are sorted by stable identity (name, then description, then
// canonical input schema JSON) so the same tool set always projects to the
// same byte sequence regardless of caller-supplied order. Keeping the provider
// prompt prefix stable is what lets automatic context caches (DeepSeek) hit.
func cloneDefinitions(
	definitions []provider.ToolDefinition,
) []provider.ToolDefinition {
	if definitions == nil {
		return nil
	}
	result := make([]provider.ToolDefinition, len(definitions))
	for index, definition := range definitions {
		result[index] = definition
		result[index].InputSchema = cloneMap(definition.InputSchema)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return toolDefinitionLess(result[i], result[j])
	})
	return result
}

func toolDefinitionLess(left, right provider.ToolDefinition) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.Description != right.Description {
		return left.Description < right.Description
	}
	return canonicalDefinitionSchema(left) < canonicalDefinitionSchema(right)
}

// canonicalDefinitionSchema renders the input schema as deterministic JSON
// (encoding/json sorts map keys at every depth) for use as a total-order
// tie-breaker. Marshal failure falls back to "" so ordering stays defined.
func canonicalDefinitionSchema(definition provider.ToolDefinition) string {
	if definition.InputSchema == nil {
		return ""
	}
	encoded, err := json.Marshal(definition.InputSchema)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneValue(item)
	}
	return result
}

func cloneValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneMap(item)
	case []any:
		result := make([]any, len(item))
		for index, child := range item {
			result[index] = cloneValue(child)
		}
		return result
	case []string:
		return append([]string{}, item...)
	case json.RawMessage:
		return append(json.RawMessage(nil), item...)
	default:
		return value
	}
}
