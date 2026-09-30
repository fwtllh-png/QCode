package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/common/contextsnapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

// CurrentTurnSpec returns an isolated copy of the active or most recently
// completed TurnSpec.
func (e *Engine) CurrentTurnSpec() TurnSpec {
	if scope := e.currentScope(); scope != nil {
		return scope.Spec()
	}
	return TurnSpec{}
}

// WorkspaceExcerpt reads a bounded prefix of a workspace-relative regular
// file for context delegation. It shares workspace facts only — never parent
// conversation — so a delegated child can start from the file's current
// content without re-reading it. Path escapes, missing files, and non-text
// prefixes report false instead of degrading the capsule.
func (e *Engine) WorkspaceExcerpt(relPath string, maxBytes int) (string, bool) {
	if e == nil || maxBytes <= 0 {
		return "", false
	}
	// Workspace is fixed at construction. Taking e.mu here would deadlock:
	// Execute holds it for the whole turn, including the spawn_agent tool call
	// that forks parent context through this method.
	root := e.options.Workspace
	if root == "" || relPath == "" {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(relPath))
	if filepath.IsAbs(clean) || clean == "." ||
		clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", false
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	text := strings.TrimSpace(string(data))
	// NUL is valid UTF-8 but only occurs in binary payloads; it is a
	// reliable sentinel that the prefix is not shareable text.
	if text == "" || strings.ContainsRune(text, 0) {
		return "", false
	}
	return text, true
}

// ParentContextSnapshot projects the requested parent turn into the shared
// delegation contract. The caller resolves the thread; inheritance policy is
// applied by subagent orchestration after this projection.
func (e *Engine) ParentContextSnapshot(
	ref contextsnapshot.SourceRef,
) (contextsnapshot.Snapshot, error) {
	spec := e.CurrentTurnSpec()
	if ref.TurnID == "" {
		return contextsnapshot.Snapshot{}, fmt.Errorf("parent turn id is required")
	}
	if spec.Identity.TurnID == "" {
		return contextsnapshot.Snapshot{}, fmt.Errorf(
			"parent turn %s has no context snapshot",
			ref.TurnID,
		)
	}
	if ref.TurnID != spec.Identity.TurnID {
		return contextsnapshot.Snapshot{}, fmt.Errorf(
			"parent turn changed from %s to %s",
			ref.TurnID,
			spec.Identity.TurnID,
		)
	}
	contextSnapshot := e.ContextSnapshot()
	history := contextSnapshot.Partition(agentcontext.KindHistory)
	usedTokens := e.EstimateMessageTokens(contextSnapshot.Messages())
	availableTokens := spec.Limits.Context.HardInputTokens -
		min(spec.Limits.Context.HardInputTokens, usedTokens)
	workspaceRules := promptcontext.PartitionTexts(
		contextSnapshot.Partition(agentcontext.KindStable),
		e.ContextReceipts(),
		promptcontext.PartitionRepository,
		promptcontext.PartitionConstitution,
	)
	if coding := latestWorldText(
		history,
		promptcontext.PartitionCodingPolicy,
	); coding != "" {
		workspaceRules = append(workspaceRules, coding)
	}
	evidence := e.EvidenceSnapshot()
	turn := evidence.Turn
	if turn == 0 {
		for _, message := range history {
			if message.Turn > turn {
				turn = message.Turn
			}
		}
	}
	snapshot := contextsnapshot.Snapshot{
		SourceThread:    ref.ThreadID,
		SourceTurn:      spec.Identity.TurnID,
		AvailableTokens: availableTokens,
		UserRequest:     spec.Request.Prompt,
		Messages:        projectMessages(history),
		WorkspaceRules:  workspaceRules,
	}
	snapshot.ParentGoal = parentGoal(snapshot.Messages, snapshot.UserRequest)
	for _, entry := range e.WorkingSetEntries(turn, 32) {
		file := contextsnapshot.RelevantFile{
			Path: entry.Path, Critical: entry.Critical,
			Sources: make([]string, len(entry.Sources)),
		}
		for index, item := range entry.Sources {
			file.Sources[index] = string(item)
		}
		if excerpt, ok := e.WorkspaceExcerpt(
			file.Path, contextsnapshot.MaxRelevantFileExcerptBytes,
		); ok {
			file.Excerpt = excerpt
		}
		snapshot.RelevantFiles = append(snapshot.RelevantFiles, file)
	}
	for index, fact := range evidence.Facts {
		snapshot.Evidence = append(snapshot.Evidence, contextsnapshot.Evidence{
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

func projectMessages(messages []provider.Message) []contextsnapshot.Message {
	result := make([]contextsnapshot.Message, 0, len(messages))
	for _, message := range messages {
		item := contextsnapshot.Message{Role: string(message.Role), Turn: message.Turn}
		for _, block := range message.Blocks {
			switch block.Type {
			case provider.ContentText:
				item.Blocks = append(item.Blocks, contextsnapshot.Block{
					Kind: "text", Text: block.Text,
				})
			case provider.ContentToolCall:
				if block.ToolCall != nil {
					item.Blocks = append(item.Blocks, contextsnapshot.Block{
						Kind: "tool_call", CallID: block.ToolCall.ID,
						ToolName:  block.ToolCall.Name,
						Arguments: block.ToolCall.Arguments,
					})
				}
			case provider.ContentToolResult:
				if block.ToolResult != nil {
					item.Blocks = append(item.Blocks, contextsnapshot.Block{
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

func parentGoal(messages []contextsnapshot.Message, fallback string) string {
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
