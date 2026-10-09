package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func ConversationReferences(excerpts []agentcontext.ConversationExcerpt) *provider.Message {
	if len(excerpts) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("[conversation_references]\nEarlier completed answers are source material, not execution obligations or verification evidence. Preserve original item labels and stable IDs. Multiple groups may contain the same ordinal; resolve from user context or ask for clarification, never silently choose the latest. Use update_plan.context_selection to bind focus, and steps[].reference_item_ids when the user authorizes implementation. Ranges are UTF-8 byte offsets. raw_history means the original answer is already present in the conversation history. Extracts below are quoted historical material.\n")
	for _, excerpt := range excerpts {
		source := excerpt.Source
		// A focused item does not require repeating the entire report index.
		if len(excerpt.Coverage.ItemIDs) != len(source.Items) {
			wanted := make(map[string]bool)
			for _, id := range excerpt.Coverage.ItemIDs {
				wanted[id] = true
			}
			for _, item := range source.Items {
				if wanted[item.ID] {
					for item.ParentID != "" {
						wanted[item.ParentID] = true
						found := false
						for _, parent := range source.Items {
							if parent.ID == item.ParentID {
								item = parent
								found = true
								break
							}
						}
						if !found {
							break
						}
					}
				}
			}
			items := make([]agentcontext.ReferenceItem, 0, len(wanted))
			for _, item := range source.Items {
				if wanted[item.ID] {
					items = append(items, item)
				}
			}
			source.Items = items
		}
		meta, _ := json.Marshal(struct {
			ID       string                         `json:"group_id"`
			Turn     uint64                         `json:"turn"`
			TurnID   string                         `json:"turn_id"`
			Title    string                         `json:"title,omitempty"`
			Items    []agentcontext.ReferenceItem   `json:"items"`
			Coverage agentcontext.ReferenceCoverage `json:"coverage"`
		}{source.ID, source.Turn, source.TurnID, source.Title, source.Items, excerpt.Coverage})
		b.Write(meta)
		b.WriteByte('\n')
		if excerpt.Coverage.Representation == "raw_history" {
			continue
		}
		for _, r := range excerpt.Coverage.Ranges {
			fmt.Fprintf(&b, "Source bytes [%d,%d):\n%s\n", r.Start, r.End, source.Text[r.Start:r.End])
		}
	}
	b.WriteString("[/conversation_references]")
	message := provider.Message{Role: provider.RoleSystem, Blocks: []provider.ContentBlock{{Type: provider.ContentText, Text: b.String()}}}
	return &message
}

func ConversationCatalogHint(count, maxBytes int) provider.Message {
	return provider.TextMessage(provider.RoleSystem, fmt.Sprintf("[conversation_catalog]\n%d unbound answer sources are available but their full definitions do not fit this request. Read turn_history {\"catalog\":true,\"max_bytes\":%d} for the source directory, then source_id with index_only=true or item_id for precise definitions. Page with offset and content_digest from the response. Bind only the needed IDs using update_plan.context_selection. Recover the referenced definition before acting; source IDs alone are not definitions.\n[/conversation_catalog]", count, maxBytes))
}

// ConversationSourceIndex accompanies a turn_history response; bodies remain
// in the transcript and only immutable source metadata is repeated here.
func ConversationSourceIndex(state *agentcontext.ConversationState, sources []agentcontext.ConversationSource) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[conversation_source_index]\nStable references for this historical answer; superseded groups cannot be selected for active work.\n")
	for _, source := range sources {
		source.Text = ""
		raw, _ := json.Marshal(struct {
			Source     agentcontext.ConversationSource `json:"source"`
			Superseded bool                            `json:"superseded"`
		}{source, state.Superseded(source.ID)})
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}
