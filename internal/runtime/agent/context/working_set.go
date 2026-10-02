// Package agentcontext keeps the paths a thread has touched, ranked by how likely
// they still matter. It is the memory behind the working set the agent sees each
// turn: the engine reports what it read, wrote, diagnosed and verified, and the
// ledger decides which of those paths are worth spending prompt bytes on.
//
// The ledger holds paths, not contents. A file the model already read is in the
// history; naming it again is a reminder, whereas re-injecting it would be paid
// for twice.
package agentcontext

import (
	"sort"
	"strings"
	"sync"
)

// WorkingSetSource is where the knowledge that a path matters came from. Sources add up:
// a file that was edited and then reported a diagnostic outranks one that was
// only read.
type WorkingSetSource string

const (
	// SourcePinned is a path the user named through a host attachment.
	SourcePinned WorkingSetSource = "pinned"
	// SourcePlan is a path the plan called critical.
	SourcePlan WorkingSetSource = "plan"
	// SourceEdited is a path a tool observably wrote.
	SourceEdited WorkingSetSource = "edited"
	// SourceDiagnostic is a path post-edit diagnostics reported on.
	SourceDiagnostic WorkingSetSource = "diagnostic"
	// SourceVerified is a path the verify gate ran over.
	SourceVerified WorkingSetSource = "verified"
	// SourceRead is a path a tool read.
	SourceRead WorkingSetSource = "read"
	// SourceSearch is a path a search matched. It is the weakest source: a hit
	// says the name occurs there, not that anyone has looked at it.
	SourceSearch WorkingSetSource = "search"
)

// Weights per source. They are relative, not absolute: only their order and
// rough spacing matter, because the score exists to sort a list that a byte
// budget then cuts.
const (
	weightEdited     = 40
	weightDiagnostic = 30
	weightVerified   = 25
	weightRead       = 10
	weightSearch     = 5
	weightPinned     = 50
	weightPlan       = 50

	// maxBase caps the sum so a path with every source cannot dwarf the rest.
	maxBase = 100

	// criticalScore lifts pinned and plan paths above everything that decays.
	criticalScore = 1 << 20

	// decayScale and decayStep divide the score by age in turns: one turn of
	// silence costs a third of the score, three turns cost more than half. The
	// arithmetic is integral so the ordering is reproducible.
	decayScale = 100
	decayStep  = 50
)

func weight(source WorkingSetSource) int {
	switch source {
	case SourcePinned:
		return weightPinned
	case SourcePlan:
		return weightPlan
	case SourceEdited:
		return weightEdited
	case SourceDiagnostic:
		return weightDiagnostic
	case SourceVerified:
		return weightVerified
	case SourceRead:
		return weightRead
	case SourceSearch:
		return weightSearch
	default:
		return 0
	}
}

// critical reports whether a source pins a path against decay. The user and the
// plan say what matters for the whole task; the engine's own observations only
// say what mattered a moment ago.
func critical(source WorkingSetSource) bool {
	return source == SourcePinned || source == SourcePlan
}

// WorkingSetEntry is one path as a caller sees it. It is a copy: mutating it does not
// touch the ledger.
type WorkingSetEntry struct {
	Path string `json:"path"`
	// Sources is every source that ever named this path, most valuable first.
	Sources []WorkingSetSource `json:"sources"`
	// FirstTurn and LastTurn bracket the turns that named the path.
	FirstTurn uint64 `json:"first_turn"`
	LastTurn  uint64 `json:"last_turn"`
	// Critical marks a path the user or the plan pinned; it never decays out.
	Critical bool `json:"critical,omitempty"`
	// Score is the relevance as of the turn Select was asked about.
	Score int `json:"score"`
}

// record is the stored form. Sources map to the last turn each one fired, which
// is what lets a caller ask "which paths did this turn read".
type record struct {
	sources   map[WorkingSetSource]uint64
	firstTurn uint64
	lastTurn  uint64
}

// WorkingSetLedger accumulates observations across the turns of one thread. A nil *WorkingSetLedger
// accepts observations and returns nothing, so a caller wired without one needs
// no special case.
type WorkingSetLedger struct {
	mu      sync.Mutex
	records map[string]*record
}

// New returns an empty ledger.
func NewWorkingSet() *WorkingSetLedger {
	return &WorkingSetLedger{records: make(map[string]*record)}
}

// Observe records that source named path during turn. Repeating an observation
// only moves the turn forward, so a file read five times costs one entry.
func (l *WorkingSetLedger) Observe(source WorkingSetSource, turn uint64, path string) {
	if l == nil || weight(source) == 0 {
		return
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.records == nil {
		l.records = make(map[string]*record)
	}
	existing, found := l.records[path]
	if !found {
		l.records[path] = &record{
			sources:   map[WorkingSetSource]uint64{source: turn},
			firstTurn: turn, lastTurn: turn,
		}
		return
	}
	existing.sources[source] = turn
	if turn < existing.firstTurn {
		existing.firstTurn = turn
	}
	if turn > existing.lastTurn {
		existing.lastTurn = turn
	}
}

// ObserveAll records one source over several paths.
func (l *WorkingSetLedger) ObserveAll(source WorkingSetSource, turn uint64, paths []string) {
	for _, path := range paths {
		l.Observe(source, turn, path)
	}
}

// Select returns the most relevant entries as of turn, best first. Ties break on
// path so the same ledger always renders the same way.
//
// limit bounds the paths the ledger discovered on its own, not the pinned ones.
// Pinning two files with a limit of two would otherwise hide the file the agent
// just edited, which is the opposite of what pinning is for.
func (l *WorkingSetLedger) Select(turn uint64, limit int) []WorkingSetEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	entries := make([]WorkingSetEntry, 0, len(l.records))
	for path, stored := range l.records {
		entries = append(entries, stored.entry(path, turn))
	}
	l.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		return entries[i].Path < entries[j].Path
	})
	if limit <= 0 {
		return entries
	}
	kept := make([]WorkingSetEntry, 0, len(entries))
	discovered := 0
	for _, entry := range entries {
		if !entry.Critical {
			if discovered == limit {
				continue
			}
			discovered++
		}
		kept = append(kept, entry)
	}
	return kept
}

// PathsObservedAt returns the paths source named during turn, sorted. The turn
// receipt uses it to report what a turn read without keeping a second ledger.
func (l *WorkingSetLedger) PathsObservedAt(source WorkingSetSource, turn uint64) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var paths []string
	for path, stored := range l.records {
		if at, found := stored.sources[source]; found && at == turn {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// PathsWithSource returns every path source has ever named, sorted.
func (l *WorkingSetLedger) PathsWithSource(source WorkingSetSource) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var paths []string
	for path, stored := range l.records {
		if _, found := stored.sources[source]; found {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// HasSource reports whether source ever named path. The evidence ledger asks it
// to tell an edit that rested on a read from one made blind.
func (l *WorkingSetLedger) HasSource(source WorkingSetSource, path string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	stored, found := l.records[strings.TrimSpace(path)]
	if !found {
		return false
	}
	_, named := stored.sources[source]
	return named
}

// Len is how many paths the ledger holds.
func (l *WorkingSetLedger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

// Clone returns an independent copy, so a forked thread inherits what the parent
// learned without sharing its future.
func (l *WorkingSetLedger) Clone() *WorkingSetLedger {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := &WorkingSetLedger{records: make(map[string]*record, len(l.records))}
	for path, stored := range l.records {
		sources := make(map[WorkingSetSource]uint64, len(stored.sources))
		for source, turn := range stored.sources {
			sources[source] = turn
		}
		copied.records[path] = &record{
			sources: sources, firstTurn: stored.firstTurn, lastTurn: stored.lastTurn,
		}
	}
	return copied
}

func (r *record) entry(path string, turn uint64) WorkingSetEntry {
	entry := WorkingSetEntry{
		Path: path, FirstTurn: r.firstTurn, LastTurn: r.lastTurn,
		Sources: make([]WorkingSetSource, 0, len(r.sources)),
	}
	base := 0
	for source := range r.sources {
		entry.Sources = append(entry.Sources, source)
		base += weight(source)
		if critical(source) {
			entry.Critical = true
		}
	}
	sort.Slice(entry.Sources, func(i, j int) bool {
		left, right := entry.Sources[i], entry.Sources[j]
		if weight(left) != weight(right) {
			return weight(left) > weight(right)
		}
		return left < right
	})
	base = min(base, maxBase)
	if entry.Critical {
		entry.Score = criticalScore + base
		return entry
	}
	age := 0
	if turn > r.lastTurn {
		age = int(turn - r.lastTurn)
	}
	entry.Score = base * decayScale / (decayScale + age*decayStep)
	return entry
}
