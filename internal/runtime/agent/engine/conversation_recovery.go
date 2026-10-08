package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	turnhistory "github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// The process-level seed is only a fallback for standalone engines. Runtime
// binds the durable Session on the invocation before opening a frozen Scope.
func (e *Engine) sessionIdentity(ctx context.Context) string {
	if scope := e.runningScope(); scope != nil && scope.spec.Identity.SessionID != "" {
		return scope.spec.Identity.SessionID
	}
	if session := tool.InvocationIdentityFrom(ctx).SessionID; session != "" {
		return session
	}
	return e.options.SessionID
}

func (e *Engine) conversationSourceAvailable(ctx context.Context, source agentcontext.ConversationSource) (bool, error) {
	if owner := e.options.SessionForTurn; owner != nil {
		session, found := owner(ctx, source.TurnID)
		if !found || session == "" || session != e.sessionIdentity(ctx) {
			return false, nil
		}
	}
	if store := e.options.TurnContexts; store != nil {
		withdrawn, err := store.TurnWithdrawn(ctx, protocol.ThreadID(source.ThreadID), protocol.TurnID(source.TurnID))
		return !withdrawn && err == nil, err
	}
	return true, nil
}

func (e *Engine) lookupConversationEntry(ctx context.Context, request turnhistory.ReferenceRequest) (*turnhistory.Entry, error) {
	state := e.contextAuthority().Conversation()
	if state == nil {
		return nil, nil
	}
	var sources []agentcontext.ConversationSource
	var item *agentcontext.ReferenceItem
	if request.Catalog {
		for _, source := range state.Sources {
			sources = append(sources, source)
		}
		sort.Slice(sources, func(i, j int) bool {
			if sources[i].Turn != sources[j].Turn {
				return sources[i].Turn < sources[j].Turn
			}
			return sources[i].ID < sources[j].ID
		})
	} else if request.SourceID != "" {
		if source, ok := state.Sources[request.SourceID]; ok {
			sources = append(sources, source)
		}
	} else if source, selected, ok := state.FindItem(request.ItemID); ok {
		sources = append(sources, source)
		item = &selected
	}
	var b strings.Builder
	for _, source := range sources {
		available, err := e.conversationSourceAvailable(ctx, source)
		if err != nil {
			return nil, err
		}
		if !available {
			continue
		}
		if request.Catalog {
			row, _ := json.Marshal(struct {
				ID         string `json:"source_id"`
				Turn       uint64 `json:"turn"`
				Title      string `json:"title,omitempty"`
				Items      int    `json:"item_count"`
				Superseded bool   `json:"superseded"`
			}{source.ID, source.Turn, source.Title, len(source.Items), state.Superseded(source.ID)})
			b.Write(row)
			b.WriteByte('\n')
			continue
		}
		ranges := []agentcontext.ReferenceRange{{Start: 0, End: len(source.Text)}}
		items := source.Items
		if item != nil {
			parent := *item
			items = []agentcontext.ReferenceItem{*item}
			for parent.ParentID != "" {
				_, ancestor, ok := state.FindItem(parent.ParentID)
				if !ok {
					return nil, fmt.Errorf("source parent is unavailable")
				}
				items = append(items, ancestor)
				parent = ancestor
			}
			ranges = agentcontext.ConversationDependencyRanges(source, []string{item.ID})
		}
		meta, _ := json.Marshal(struct {
			SourceID   string                        `json:"source_id"`
			Digest     string                        `json:"source_digest"`
			Ranges     []agentcontext.ReferenceRange `json:"source_ranges"`
			Items      []agentcontext.ReferenceItem  `json:"items"`
			Superseded bool                          `json:"superseded"`
		}{source.ID, source.ContentDigest, ranges, items, state.Superseded(source.ID)})
		b.Write(meta)
		b.WriteByte('\n')
		if !request.IndexOnly {
			for _, r := range ranges {
				fmt.Fprintf(&b, "Source bytes [%d,%d):\n", r.Start, r.End)
				b.WriteString(source.Text[r.Start:r.End])
				b.WriteByte('\n')
			}
		}
	}
	if b.Len() == 0 && !request.Catalog {
		return nil, nil
	}
	return &turnhistory.Entry{Transcript: b.String()}, nil
}
