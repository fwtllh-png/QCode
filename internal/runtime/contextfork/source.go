package contextfork

import (
	"context"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

type EngineResolver interface {
	ContextEngine(string) (*agentengine.Engine, error)
}

type Source struct {
	resolver EngineResolver
}

func NewSource(resolver EngineResolver) *Source {
	return &Source{resolver: resolver}
}

func (s *Source) Snapshot(
	_ context.Context,
	ref ContextSourceRef,
) (ParentContextSnapshot, error) {
	if s == nil || s.resolver == nil {
		return ParentContextSnapshot{}, fmt.Errorf(
			"parent thread resolver is unavailable",
		)
	}
	engine, err := s.resolver.ContextEngine(ref.ThreadID)
	if err != nil {
		return ParentContextSnapshot{}, err
	}
	spec := engine.CurrentTurnSpec()
	if ref.TurnID == "" {
		return ParentContextSnapshot{}, fmt.Errorf("parent turn id is required")
	}
	if spec.Identity.TurnID == "" {
		return ParentContextSnapshot{}, fmt.Errorf(
			"parent turn %s has no context snapshot",
			ref.TurnID,
		)
	}
	if ref.TurnID != spec.Identity.TurnID {
		return ParentContextSnapshot{}, fmt.Errorf(
			"parent turn changed from %s to %s",
			ref.TurnID,
			spec.Identity.TurnID,
		)
	}
	contextSnapshot := engine.ContextSnapshot()
	history := contextSnapshot.Partition(agentcontext.KindHistory)
	usedTokens := engine.EstimateMessageTokens(contextSnapshot.Messages())
	availableTokens := spec.Limits.Context.HardInputTokens -
		min(spec.Limits.Context.HardInputTokens, usedTokens)
	workspaceRules := promptcontext.PartitionTexts(
		contextSnapshot.Partition(agentcontext.KindStable),
		engine.ContextReceipts(),
		promptcontext.PartitionRepository,
		promptcontext.PartitionConstitution,
	)
	if coding := latestWorldText(
		history,
		promptcontext.PartitionCodingPolicy,
	); coding != "" {
		workspaceRules = append(workspaceRules, coding)
	}
	evidence := engine.EvidenceSnapshot()
	turn := evidence.Turn
	if turn == 0 {
		for _, message := range history {
			if message.Turn > turn {
				turn = message.Turn
			}
		}
	}
	snapshot := ParentContextSnapshot{
		SourceThread:    ref.ThreadID,
		SourceTurn:      spec.Identity.TurnID,
		AvailableTokens: availableTokens,
		UserRequest:     spec.Request.Prompt,
		Messages:        projectMessages(history),
		WorkspaceRules:  workspaceRules,
	}
	snapshot.ParentGoal = parentGoal(snapshot.Messages, snapshot.UserRequest)
	for _, entry := range engine.WorkingSetEntries(turn, 32) {
		file := ContextRelevantFile{
			Path: entry.Path, Critical: entry.Critical,
			Sources: make([]string, len(entry.Sources)),
		}
		for index, item := range entry.Sources {
			file.Sources[index] = string(item)
		}
		if excerpt, ok := engine.WorkspaceExcerpt(
			file.Path, MaxRelevantFileExcerptBytes,
		); ok {
			file.Excerpt = excerpt
		}
		snapshot.RelevantFiles = append(snapshot.RelevantFiles, file)
	}
	for index, fact := range evidence.Facts {
		snapshot.Evidence = append(snapshot.Evidence, ContextEvidence{
			Summary: fact.Describe(),
			Handle: fmt.Sprintf(
				"evidence://%s/%s/%d",
				ref.ThreadID,
				snapshot.SourceTurn,
				index+1,
			),
		})
	}
	return snapshot, nil
}

func latestWorldText(
	messages []provider.Message,
	id string,
) string {
	var result string
	for _, message := range messages {
		entry, _, ok := agentcontext.InspectWorldMessage(message)
		if !ok || entry.ID != id {
			continue
		}
		if entry.Present {
			result = message.Text()
		} else {
			result = ""
		}
	}
	return result
}

func projectMessages(messages []provider.Message) []ContextMessage {
	result := make([]ContextMessage, 0, len(messages))
	for _, message := range messages {
		item := ContextMessage{Role: string(message.Role), Turn: message.Turn}
		for _, block := range message.Blocks {
			switch block.Type {
			case provider.ContentText:
				item.Blocks = append(item.Blocks, ContextBlock{
					Kind: "text", Text: block.Text,
				})
			case provider.ContentToolCall:
				if block.ToolCall != nil {
					item.Blocks = append(item.Blocks, ContextBlock{
						Kind: "tool_call", CallID: block.ToolCall.ID,
						ToolName:  block.ToolCall.Name,
						Arguments: block.ToolCall.Arguments,
					})
				}
			case provider.ContentToolResult:
				if block.ToolResult != nil {
					item.Blocks = append(item.Blocks, ContextBlock{
						Kind: "tool_result", CallID: block.ToolResult.CallID,
						Text:    block.ToolResult.Content,
						IsError: block.ToolResult.IsError,
					})
				}
			}
		}
		if len(item.Blocks) != 0 {
			result = append(result, item)
		}
	}
	return result
}

func parentGoal(messages []ContextMessage, fallback string) string {
	for _, message := range messages {
		if message.Role != string(provider.RoleUser) {
			continue
		}
		for _, block := range message.Blocks {
			if block.Kind == "text" && block.Text != "" {
				return block.Text
			}
		}
	}
	return fallback
}
