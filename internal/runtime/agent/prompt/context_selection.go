package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// ContextSelectionHint renders only the selected projection's omissions. Runs
// aggregate consecutive turns with the same cause; sparse turn numbers never
// become a fabricated recoverable interval. This message is ephemeral input,
// not a world fact persisted into later samples.
func ContextSelectionHint(result agentcontext.ProjectionResult, maxBytes int) (*provider.Message, error) {
	type run struct {
		first, last uint64
		reason      agentcontext.OmissionReason
	}
	var runs []run
	for _, omission := range result.Omissions {
		if omission.Retrieval == nil {
			continue
		}
		turn := omission.Retrieval.Turn
		if len(runs) != 0 {
			last := &runs[len(runs)-1]
			if last.reason == omission.Reason && (turn == last.last || turn == last.last+1) {
				last.last = turn
				continue
			}
		}
		runs = append(runs, run{first: turn, last: turn, reason: omission.Reason})
	}
	if len(runs) == 0 {
		return nil, nil
	}
	render := func(first int, compact bool) string {
		var text strings.Builder
		text.WriteString("[context_selection]\n")
		if !compact {
			text.WriteString("The following raw history is omitted from this request. Use definitions already supplied in conversation_references directly; an omitted raw turn does not require recovery when its definition is covered. Recover only missing definitions or evidence needed for the current request. Do not search the repository for conversation-only lists.\n")
		}
		if first > 0 {
			fmt.Fprintf(&text, "%d additional omission groups are not listed in this hint.\n", first)
		}
		for _, run := range runs[first:] {
			arguments, _ := json.Marshal(agentcontext.TurnRetrieval{Turn: run.last})
			if run.first == run.last {
				fmt.Fprintf(&text, "turn=%d", run.first)
			} else {
				fmt.Fprintf(&text, "turns=%d-%d", run.first, run.last)
			}
			fmt.Fprintf(&text, " reason=%s; %s %s\n", run.reason, agentcontext.TurnHistoryToolName, arguments)
		}
		if !compact {
			text.WriteString("For other turns in a listed range, set turn to that turn number. The first page ends with findings and sites. If truncated, page with result_get mode=tail or mode=query (for example query=sites).")
		}
		return text.String()
	}
	text := render(0, false)
	if maxBytes > 0 && len(text) > maxBytes {
		for first := 0; first < len(runs); first++ {
			text = render(first, true)
			if len(text) <= maxBytes {
				break
			}
		}
		if len(text) > maxBytes {
			return nil, fmt.Errorf("context selection hint needs %d bytes, configured session state limit is %d", len(text), maxBytes)
		}
	}
	message := provider.TextMessage(provider.RoleSystem, text)
	return &message, nil
}
