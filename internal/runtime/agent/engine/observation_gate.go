package engine

import (
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func (e *Engine) observationGate(
	call provider.ToolCall,
	_ bool,
) *tool.Result {
	if call.Name != "file_read" {
		return nil
	}
	path, startLine, ok := turnkernel.ParseFileReadWindow(call.Arguments)
	if !ok {
		return nil
	}
	item, _ := e.admissionWorkItem()
	read, known := e.knownWorkItemRead(item, path)
	if known {
		replay, invalidation := e.replayCoveredRead(read, path, startLine, call)
		if replay != nil {
			return replay
		}
		// The record cannot answer this read; the necessary read is
		// admitted and the invalidation reason is stamped onto its result
		// so the refresh is explained rather than silent.
		e.noteReadInvalidation(call.ID, invalidation)
	}
	return nil
}

func (e *Engine) admissionWorkItem() (turnkernel.WorkItem, bool) {
	kernel := e.admissionKernel
	if kernel == nil {
		if scope := e.executionScope(); scope != nil {
			scope.mu.Lock()
			kernel = scope.state.kernel
			scope.mu.Unlock()
		}
	}
	if kernel == nil {
		return turnkernel.WorkItem{}, false
	}
	return kernel.WorkItem(), kernel.RecoveryContinue()
}

func (e *Engine) knownWorkItemRead(
	item turnkernel.WorkItem,
	path string,
) (turnkernel.WorkItemRead, bool) {
	path = strings.TrimSpace(path)
	if read, ok := item.KnownRead(path); ok {
		return read, true
	}
	if relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, path); ok {
		return item.KnownRead(relative)
	}
	return turnkernel.WorkItemRead{}, false
}

func (e *Engine) locatedReadLine(path string) (int, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, false
	}
	if relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, path); ok {
		path = relative
	}
	line := 0
	for _, fact := range e.EvidenceSnapshot().Facts {
		if fact.Path == path && fact.Line > 0 &&
			(line == 0 || fact.Line < line) {
			line = fact.Line
		}
	}
	return line, line > 0
}

func (e *Engine) locatedSites() []string {
	var sites []string
	seen := make(map[string]struct{})
	for _, fact := range e.EvidenceSnapshot().Facts {
		if fact.Line <= 0 || strings.TrimSpace(fact.Path) == "" {
			continue
		}
		site := fmt.Sprintf("%s:%d", fact.Path, fact.Line)
		if _, exists := seen[site]; exists {
			continue
		}
		seen[site] = struct{}{}
		sites = append(sites, site)
	}
	return sites
}
