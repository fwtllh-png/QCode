package engine

import (
	"maps"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

// readResultEntry is the session-scoped reuse record of one file_read. Only
// reads whose dependency version the guard recorded (a workspace-read digest
// fact) may enter: a result without a verifiable version has no reuse
// contract and must re-execute.
type readResultEntry struct {
	ContentDigest   string
	RequestIdentity string
	CallID          string
	Turn            uint64
	Result          tool.Result
}

// recordReadResults admits accepted file_read results into the cross-turn
// reuse cache. The digest fact is the dependency version; without it the
// entry is skipped so arbitrary outputs never become replayable reads.
func (e *Engine) recordReadResults(
	calls []provider.ToolCall,
	results []tool.Result,
) {
	for index, call := range calls {
		if index >= len(results) || call.Name != "file_read" ||
			results[index].IsError {
			continue
		}
		result := results[index]
		if result.Outcome == nil || result.Outcome.Facts == nil ||
			result.Outcome.Facts.WorkspaceRead == nil {
			continue
		}
		fact := result.Outcome.Facts.WorkspaceRead
		path := strings.TrimSpace(fact.Path)
		if path == "" {
			continue
		}
		if relative, ok := agentcontext.WorkspaceRelative(
			e.options.Workspace, path,
		); ok {
			path = relative
		}
		entry := readResultEntry{
			ContentDigest:   strings.TrimSpace(fact.Digest),
			RequestIdentity: readRequestIdentity(call),
			CallID:          call.ID,
			Turn:            e.turn,
			// Retain the read surface, not the original execution authority.
			Result: tool.Result{
				Content: result.Content, Metadata: maps.Clone(result.Metadata),
				Truncated: result.Truncated, OriginalBytes: result.OriginalBytes,
				Handle: result.Handle,
				Outcome: &tool.Outcome{
					Status: tool.OutcomeSucceeded,
					Facts: &tool.OutcomeFacts{
						WorkspaceRead: &tool.WorkspaceReadFact{Path: fact.Path, Digest: fact.Digest},
						ResultHandle:  result.Outcome.Facts.ResultHandle,
					},
				},
			},
		}
		if entry.ContentDigest == "" {
			continue
		}
		e.readResultMu.Lock()
		if existing, exists := e.readResults[path]; !exists ||
			entry.Turn >= existing.Turn {
			e.readResults[path] = entry
		}
		e.readResultMu.Unlock()
	}
}

func readRequestIdentity(call provider.ToolCall) string {
	return turnkernel.FormatToolCallsIdentity([]turnkernel.ToolCallState{{
		Name: call.Name, Arguments: call.Arguments,
	}})
}

// replayCoveredRead serves the recorded result of an unchanged file whose
// normalized request matches exactly. A nil return means the record cannot
// answer this read; the second return explains which guarantee failed so the
// caller can record the invalidation on the admitted fresh read.
func (e *Engine) replayCoveredRead(
	read turnkernel.WorkItemRead,
	path string,
	startLine int,
	call provider.ToolCall,
) (*tool.Result, string) {
	if read.ContentDigest == "" {
		return nil, "read_record_has_no_content_version"
	}
	if !turnkernel.ReadWindowCovers(read, startLine) {
		return nil, "requested_window_not_covered"
	}
	e.readResultMu.Lock()
	entry, cached := e.readResults[path]
	e.readResultMu.Unlock()
	if !cached || entry.ContentDigest != read.ContentDigest {
		return nil, "recorded_result_unavailable"
	}
	// A matching start alone does not prove coverage of max_lines or PDF pages.
	// Reusing a different window would also relabel its body in the reducer.
	if entry.RequestIdentity != readRequestIdentity(call) {
		return nil, "requested_window_not_covered"
	}
	current, err := e.currentWorkspaceDigest(path)
	if err != nil {
		return nil, "content_version_unverifiable"
	}
	if current != read.ContentDigest {
		return nil, "file_content_changed"
	}
	result := entry.Result
	result.Outcome = tool.CloneOutcome(entry.Result.Outcome)
	result.Metadata = maps.Clone(entry.Result.Metadata)
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["reused_read"] = true
	result.Metadata["source_call_id"] = entry.CallID
	result.Metadata["source_turn"] = entry.Turn
	result.Metadata["content_digest"] = read.ContentDigest
	return &result, ""
}

// noteReadInvalidation records why a known read record could not answer an
// admitted request; BeforeClose stamps it onto the fresh result so the
// invalidation is explained where the model sees it.
func (e *Engine) noteReadInvalidation(callID, reason string) {
	if callID == "" || reason == "" {
		return
	}
	e.readResultMu.Lock()
	if e.readInvalidations == nil {
		e.readInvalidations = make(map[string]string)
	}
	e.readInvalidations[callID] = reason
	e.readResultMu.Unlock()
}

func (e *Engine) takeReadInvalidation(callID string) string {
	if callID == "" {
		return ""
	}
	e.readResultMu.Lock()
	reason := e.readInvalidations[callID]
	delete(e.readInvalidations, callID)
	e.readResultMu.Unlock()
	return reason
}

func (e *Engine) currentWorkspaceDigest(path string) (string, error) {
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(e.options.Workspace, resolved)
	}
	fingerprint, _, _, err := workspacejournal.Snapshot(resolved)
	if err != nil || !fingerprint.Exists {
		return "", err
	}
	return fingerprint.SHA256, nil
}
