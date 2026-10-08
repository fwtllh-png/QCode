package turnkernel

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// TurnDiffEntry is one path a tool observably changed in the active turn (N18).
type TurnDiffEntry struct {
	Path string `json:"path"`
	Tool string `json:"tool"`
	// Kind is created | modified | deleted, as observed by the guard.
	Kind string `json:"kind,omitempty"`
	// Added and Removed are the turn's cumulative line delta for the path.
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TurnDiffTracker accumulates net file-tool changes for the current turn.
type TurnDiffTracker struct {
	mu        sync.Mutex
	entries   map[string]TurnDiffEntry
	workspace string
}

func NewTurnDiffTracker(workspace ...string) *TurnDiffTracker {
	tracker := &TurnDiffTracker{entries: make(map[string]TurnDiffEntry)}
	if len(workspace) != 0 {
		tracker.workspace = workspace[0]
	}
	return tracker
}

func (t *TurnDiffTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[string]TurnDiffEntry)
}

func (t *TurnDiffTracker) Record(entry TurnDiffEntry) {
	if t == nil {
		return
	}
	entry.Path = strings.TrimSpace(entry.Path)
	if entry.Path == "" {
		return
	}
	entry.Path = filepath.Clean(entry.Path)
	if filepath.IsAbs(entry.Path) && t.workspace != "" {
		if relative, err := filepath.Rel(t.workspace, entry.Path); err == nil &&
			relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			entry.Path = relative
		}
	}
	entry.Path = filepath.ToSlash(entry.Path)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]TurnDiffEntry)
	}
	if previous, exists := t.entries[entry.Path]; exists {
		switch {
		case previous.Kind == tool.WorkspaceCreated && entry.Kind == tool.WorkspaceDeleted:
			delete(t.entries, entry.Path)
			return
		case previous.Kind == tool.WorkspaceCreated:
			entry.Kind = tool.WorkspaceCreated
		case previous.Kind == tool.WorkspaceDeleted && entry.Kind == tool.WorkspaceCreated:
			entry.Kind = tool.WorkspaceModified
		}
		entry.Added += previous.Added
		entry.Removed += previous.Removed
	}
	t.entries[entry.Path] = entry
}

// Replace freezes a reconciled net diff for receipts after journal settlement.
func (t *TurnDiffTracker) Replace(entries []TurnDiffEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[string]TurnDiffEntry, len(entries))
	for _, entry := range entries {
		t.entries[entry.Path] = entry
	}
}

func (t *TurnDiffTracker) Snapshot() []TurnDiffEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TurnDiffEntry, 0, len(t.entries))
	for _, entry := range t.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (t *TurnDiffTracker) Format() string {
	entries := t.Snapshot()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("turn-diff:\n")
	for _, entry := range entries {
		b.WriteString("  ")
		b.WriteString(entry.Path)
		if entry.Tool != "" {
			b.WriteString(" (")
			b.WriteString(entry.Tool)
			b.WriteString(")")
		}
		if entry.Kind != "" {
			b.WriteString(" ")
			b.WriteString(entry.Kind)
		}
		if entry.Added != 0 || entry.Removed != 0 {
			fmt.Fprintf(&b, " +%d -%d", entry.Added, entry.Removed)
		}
		if entry.Summary != "" {
			b.WriteString(" — ")
			b.WriteString(entry.Summary)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// observedFileChanges reads the guard's write observations off a tool result.
// A tool that changed nothing (or declared no write resources) carries none.
func ObservedFileChanges(result tool.Result) []tool.WorkspaceChange {
	if result.Outcome == nil || result.Outcome.Facts == nil {
		return nil
	}
	return result.Outcome.Facts.WorkspaceChanges
}

// observedFileRead reads the path a read-tracked tool fingerprinted, which is the
// only record that a tool read a file rather than wrote one. It is absolute, as
// the fingerprint is.
func ObservedFileRead(result tool.Result) string {
	if result.Outcome == nil || result.Outcome.Facts == nil ||
		result.Outcome.Facts.WorkspaceRead == nil {
		return ""
	}
	return result.Outcome.Facts.WorkspaceRead.Path
}
