package agentcontext

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
)

var resultHandleTools = map[string]struct{}{
	"result_get": {}, "handle_read": {},
}

func WorkspaceRelative(workspace string, path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return "", false
	}
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(filepath.Clean(path)), true
	}
	if workspace == "" {
		return "", false
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", false
	}
	roots := []string{filepath.Clean(absolute)}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil &&
		resolved != roots[0] {
		roots = append(roots, resolved)
	}
	clean := filepath.Clean(path)
	for _, root := range roots {
		relative, err := filepath.Rel(root, clean)
		if err != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if relative == "." {
			return "", false
		}
		return filepath.ToSlash(relative), true
	}
	// A deleted path may no longer exist, but its nearest surviving parent can
	// still resolve an alternate spelling of the workspace root.
	resolved, err := resolveMissingWorkspacePath(clean)
	if err != nil || resolved == clean {
		return "", false
	}
	relative, err := filepath.Rel(roots[len(roots)-1], resolved)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func (a *Authority) ObservePath(
	workspace string,
	source WorkingSetSource,
	turn uint64,
	path string,
) {
	if relative, ok := WorkspaceRelative(workspace, path); ok {
		a.WorkingSet().Observe(source, turn, relative)
	}
}

func (a *Authority) NoteToolCall(call provider.ToolCall) {
	a.Evidence().NoteCall(call.Name, call.Arguments)
	if _, found := resultHandleTools[call.Name]; !found {
		return
	}
	var arguments struct {
		Handle string `json:"handle"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
		return
	}
	for _, candidate := range []string{arguments.Handle, arguments.ID} {
		a.Evidence().ConsumeHandle(candidate)
	}
}

func (a *Authority) ObserveToolResult(
	workspace string,
	call provider.ToolCall,
	result tool.Result,
	turn uint64,
) {
	if result.Outcome == nil || result.Outcome.Facts == nil {
		return
	}
	facts := result.Outcome.Facts
	for _, hit := range facts.Evidence {
		path, ok := WorkspaceRelative(workspace, hit.Path)
		if !ok {
			continue
		}
		a.Evidence().Observe(EvidenceFact{
			Kind: EvidenceKind(hit.Kind), Path: path, Line: hit.Line,
			Symbol: hit.Symbol, Tool: call.Name, Turn: turn,
		})
		a.WorkingSet().Observe(SourceSearch, turn, path)
	}
	if facts.WorkspaceRead != nil {
		if path, ok := WorkspaceRelative(
			workspace,
			facts.WorkspaceRead.Path,
		); ok {
			a.Evidence().NoteRead(path, facts.WorkspaceRead.Digest)
		}
	}
	if facts.ResultHandle != "" {
		a.Evidence().NoteHandle(facts.ResultHandle, call.Name)
	}
}

func (a *Authority) ObserveToolFailure(
	call provider.ToolCall,
	result tool.Result,
	turn uint64,
) {
	category := ""
	if result.Outcome != nil && result.Outcome.Facts != nil &&
		result.Outcome.Facts.Failure != nil &&
		result.Outcome.Facts.Failure.Category != "" {
		category = result.Outcome.Facts.Failure.Category + ": "
	}
	reason := category + result.Content
	if result.Outcome != nil && result.Outcome.Facts != nil &&
		result.Outcome.Facts.ProcessSession != nil {
		process := result.Outcome.Facts.ProcessSession
		prefix := category + fmt.Sprintf("process exit=%d terminated=%t: ", process.ExitCode, process.Terminated)
		tailBudget := max(0, failureReasonBytes-len(prefix)-3)
		tail := strings.TrimSpace(result.Content)
		if len(tail) > tailBudget {
			tail = strings.ToValidUTF8(tail[len(tail)-tailBudget:], "")
		}
		reason = prefix + tail + "\n" + result.Content
	}
	a.Failures().NoteToolCall(turn, call.Name, call.ID, reason)
}

func (a *Authority) ObserveChange(
	workspace string,
	change tool.WorkspaceChange,
	turn uint64,
) {
	path, ok := WorkspaceRelative(workspace, change.Path)
	if !ok {
		return
	}
	read := change.Kind == tool.WorkspaceCreated ||
		a.WorkingSet().HasSource(SourceRead, path)
	a.Evidence().MarkChanged(path, turn, read)
}

func (a *Authority) ObserveDiagnostics(
	workspace string,
	receipts []diagnostics.Receipt,
) {
	for _, receipt := range receipts {
		if receipt.Status == "unavailable" {
			continue
		}
		if path, ok := WorkspaceRelative(workspace, receipt.Path); ok {
			a.Evidence().MarkDiagnostics(
				path,
				len(receipt.Diagnostics) > 0,
			)
		}
	}
}

func (a *Authority) ObserveVerified(
	workspace string,
	paths []string,
) {
	relative := make([]string, 0, len(paths))
	for _, path := range paths {
		if candidate, ok := WorkspaceRelative(workspace, path); ok {
			relative = append(relative, candidate)
		}
	}
	a.Evidence().MarkVerified(relative)
}
