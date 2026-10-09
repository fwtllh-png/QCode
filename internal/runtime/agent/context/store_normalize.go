package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type NormalizationReceipt struct {
	ToolCalls           int
	ToolResults         int
	PairedCalls         int
	DroppedOrphans      int
	ProjectedImages     int
	DroppedReasoning    int
	ModelVisibleOrphans int
}

type blockPosition struct {
	item  int
	block int
	order int
}

func NormalizePairs(
	messages []provider.Message,
) ([]provider.Message, NormalizationReceipt, error) {
	normalized, receipt, err := NewMessageLedger(LedgerInput{
		History: messages,
	}).Snapshot().Normalize(model.Capabilities{
		Reasoning: true, Vision: true, ImageInput: true, ToolCalls: true,
	})
	if err != nil {
		return nil, NormalizationReceipt{}, err
	}
	return normalized.Partition(KindHistory), receipt, nil
}

// Normalize applies provider-independent pairing and modality rules to the
// immutable MessageSnapshot used for both attribution and provider projection.
func (s MessageSnapshot) Normalize(
	capabilities model.Capabilities,
) (MessageSnapshot, NormalizationReceipt, error) {
	calls, results, validPairs, err := scanToolPositions(s.items)
	if err != nil {
		return MessageSnapshot{}, NormalizationReceipt{}, err
	}
	receipt := NormalizationReceipt{
		ToolCalls:   lenPositions(calls),
		ToolResults: lenPositions(results),
		PairedCalls: len(validPairs),
	}
	normalized := MessageSnapshot{
		revision:          s.revision,
		partitions:        make(map[MessageKind][]provider.Message, len(orderedKinds)),
		definitions:       cloneDefinitions(s.definitions),
		dynamicBeforeTurn: s.dynamicBeforeTurn,
	}
	for _, source := range s.items {
		item := source
		item.Message = CloneMessage(source.Message)
		blocks := item.Message.Blocks
		item.Message.Blocks = blocks[:0]
		changed := false
		hasRetainedToolCall := messageHasRetainedToolCall(
			source.Message,
			validPairs,
		)
		preserveReplay := replaySurvivesNormalization(
			source.Message,
			capabilities,
			validPairs,
		)
		for _, block := range blocks {
			if block.ToolCall != nil {
				if _, ok := validPairs[block.ToolCall.ID]; !ok {
					receipt.DroppedOrphans++
					changed = true
					continue
				}
			}
			if block.ToolResult != nil {
				if _, ok := validPairs[block.ToolResult.CallID]; !ok {
					receipt.DroppedOrphans++
					changed = true
					continue
				}
			}
			switch block.Type {
			case provider.ContentImage:
				if !capabilities.ImageInput && !capabilities.Vision {
					block = imagePlaceholder(*block.Attachment)
					receipt.ProjectedImages++
					changed = true
				}
			case provider.ContentReasoning:
				dropReasoning := !capabilities.Reasoning &&
					!hasRetainedToolCall && !preserveReplay
				if dropReasoning {
					receipt.DroppedReasoning++
					changed = true
					continue
				}
			}
			item.Message.Blocks = append(item.Message.Blocks, block)
		}
		clear(blocks[len(item.Message.Blocks):])
		if len(item.Message.Blocks) == 0 {
			continue
		}
		if changed && item.Message.Provenance != nil {
			item.Message.Provenance.Replay = nil
		}
		normalized.items = append(normalized.items, item)
		// Both indexes belong to this immutable snapshot; accessors copy
		// nested content before exposing it to callers.
		normalized.partitions[item.Kind] = append(
			normalized.partitions[item.Kind],
			item.Message,
		)
	}
	receipt.ModelVisibleOrphans = visibleOrphanCount(normalized.items)
	return normalized, receipt, nil
}

func validRole(role provider.Role) bool {
	switch role {
	case provider.RoleSystem, provider.RoleUser,
		provider.RoleAssistant, provider.RoleTool:
		return true
	default:
		return false
	}
}

func lenPositions(values map[string][]blockPosition) int {
	total := 0
	for _, positions := range values {
		total += len(positions)
	}
	return total
}

func validateBlockShape(block provider.ContentBlock) error {
	pointers := 0
	for _, present := range []bool{
		block.ToolCall != nil,
		block.ToolResult != nil,
		block.Search != nil,
		block.Citation != nil,
		block.Attachment != nil,
	} {
		if present {
			pointers++
		}
	}
	switch block.Type {
	case provider.ContentText, provider.ContentReasoning:
		if pointers != 0 {
			return errors.New("text block has structured payload")
		}
	case provider.ContentToolCall:
		if pointers != 1 || block.ToolCall == nil || block.Text != "" {
			return errors.New("tool call block has conflicting payload")
		}
	case provider.ContentToolResult:
		if pointers != 1 || block.ToolResult == nil || block.Text != "" {
			return errors.New("tool result block has conflicting payload")
		}
	case provider.ContentSearch:
		if pointers != 1 || block.Search == nil || block.Text != "" {
			return errors.New("search block has conflicting payload")
		}
	case provider.ContentCitation:
		if pointers != 1 || block.Citation == nil || block.Text != "" {
			return errors.New("citation block has conflicting payload")
		}
	case provider.ContentImage:
		if pointers != 1 || block.Attachment == nil || block.Text != "" {
			return errors.New("image block has conflicting payload")
		}
	}
	return nil
}

func visibleOrphanCount(items []MessageItem) int {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, item := range items {
		for _, block := range item.Message.Blocks {
			if block.ToolCall != nil {
				calls[block.ToolCall.ID]++
			}
			if block.ToolResult != nil {
				results[block.ToolResult.CallID]++
			}
		}
	}
	orphans := 0
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			orphans += count
		}
	}
	for id, count := range results {
		if count != 1 || calls[id] != 1 {
			orphans += count
		}
	}
	return orphans
}

func imagePlaceholder(attachment provider.Attachment) provider.ContentBlock {
	sum := sha256.Sum256(attachment.Data)
	return provider.ContentBlock{
		Type: provider.ContentText,
		Text: fmt.Sprintf(
			"[image omitted: model has no image-input capability; "+
				"name=%q media_type=%q bytes=%d sha256=%s]",
			attachment.Name,
			attachment.MediaType,
			len(attachment.Data),
			hex.EncodeToString(sum[:]),
		),
	}
}
