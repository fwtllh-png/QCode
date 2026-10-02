// Package agentcontext keeps what a thread found out about the repository and what
// it has not yet proved.
//
// It holds three things, and the difference between them matters:
//
//   - facts: observations a tool actually produced — this path declares that
//     symbol, that path references it, this one is its test. A fact is recorded
//     because a tool reported it, never because the model claimed it.
//   - risks: what the facts do not cover. A path the turn changed but nothing
//     verified is a risk; so is one changed without ever being read, or one whose
//     diagnostics are still open. Risks are derived, so they clear themselves the
//     moment the missing evidence arrives.
//   - reminders: call patterns that waste a turn — the same search twice, a file
//     re-read unchanged, a truncated result nobody ever retrieved. They are
//     advice, not enforcement: nothing here refuses a tool call.
//
// Like the working set, the set holds names and provenance rather than contents,
// lives in memory only, and starts empty after a resume. A nil *EvidenceSet accepts every
// observation and reports nothing, so a caller wired without one needs no special
// case.
package agentcontext

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// EvidenceKind is what a fact says about a path. The classification is lexical: it comes
// from which tool produced the hit and how the path is spelled, not from a type
// checker, so it can be wrong about a file that breaks its language's naming
// conventions.
type EvidenceKind string

const (
	// KindDefinition is a declaration site reported by a symbol lookup.
	KindDefinition EvidenceKind = "definition"
	// KindReference is a use site reported by a reference search.
	KindReference EvidenceKind = "reference"
	// KindTest is a test file, either mapped from a source file or recognized by
	// its name.
	KindTest EvidenceKind = "test"
	// KindConfig is a build or configuration file.
	KindConfig EvidenceKind = "config"
	// KindTextMatch is a plain text hit that is none of the above.
	KindTextMatch EvidenceKind = "text_match"
)

// order is the sequence a snapshot presents kinds in, most specific first: a
// declaration site is worth more prompt bytes than a grep hit.
var order = []EvidenceKind{KindDefinition, KindReference, KindTest, KindConfig, KindTextMatch}

func (k EvidenceKind) rank() int {
	for index, candidate := range order {
		if candidate == k {
			return index
		}
	}
	return len(order)
}

func (k EvidenceKind) valid() bool { return k.rank() < len(order) }

// EvidenceFact is one thing a tool reported about a path.
type EvidenceFact struct {
	Kind EvidenceKind `json:"kind"`
	Path string       `json:"path"`
	// Line is the 1-based line the hit was on, zero when the hit is the file.
	Line int `json:"line,omitempty"`
	// Symbol names the declaration a symbol lookup matched.
	Symbol string `json:"symbol,omitempty"`
	// Tool is the tool that produced the fact, so a reader can judge it.
	Tool string `json:"tool,omitempty"`
	// Turn is the turn the fact was first observed.
	Turn uint64 `json:"turn"`
	// Stale means the fact was restored against a different workspace binding.
	Stale bool `json:"stale,omitempty"`
}

// Describe renders the fact for a model to read. It lives here so the volatile
// evidence section and a compaction summary spell the same fact the same way: two
// spellings would read as two findings.
func (f EvidenceFact) Describe() string {
	location := f.Path
	if f.Line > 0 {
		location = fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	line := fmt.Sprintf("%s %s", location, f.Kind)
	if f.Stale {
		line += " [stale; re-read required]"
	}
	if f.Symbol != "" {
		line += " " + f.Symbol
	}
	if f.Tool != "" {
		line += fmt.Sprintf(" (%s, turn %d)", f.Tool, f.Turn)
	}
	return line
}

// EvidenceRisk kinds.
const (
	// RiskUnverifiedChange is a changed path no verification has covered.
	RiskUnverifiedChange = "changed_without_verification"
	// RiskBlindChange is a path modified without ever being read. The guard
	// normally refuses such a write, so this only appears where read-before-edit
	// does not apply.
	RiskBlindChange = "changed_without_read"
	// RiskOpenDiagnostics is a changed path whose diagnostics are still failing.
	RiskOpenDiagnostics = "unresolved_diagnostics"
)

// EvidenceRisk is one gap between what the thread changed and what it proved.
type EvidenceRisk struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	// Turn is the turn that introduced the risk.
	Turn uint64 `json:"turn"`
}

// EvidenceReminder kinds.
const (
	// ReminderRepeatedCall is the same tool called twice with the same arguments
	// inside one turn.
	ReminderRepeatedCall = "repeated_call"
	// ReminderRepeatedRead is a file read again whose content had not changed.
	ReminderRepeatedRead = "repeated_read"
	// ReminderUnconsumedResult is a truncated result whose handle was never read.
	ReminderUnconsumedResult = "unconsumed_result"
)

// EvidenceReminder is one wasteful call pattern, phrased for the model.
type EvidenceReminder struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// EvidenceSnapshot is the set as of one sample. It is a copy: nothing in it aliases the
// live set.
type EvidenceSnapshot struct {
	Turn      uint64             `json:"turn"`
	Facts     []EvidenceFact     `json:"facts,omitempty"`
	Risks     []EvidenceRisk     `json:"risks,omitempty"`
	Reminders []EvidenceReminder `json:"reminders,omitempty"`
	// OmittedFacts is how many facts the limit left out, so a reader can tell a
	// short list from a complete one.
	OmittedFacts int `json:"omitted_facts,omitempty"`
}

// Empty reports whether the snapshot has nothing to say.
func (s EvidenceSnapshot) Empty() bool {
	return len(s.Facts) == 0 && len(s.Risks) == 0 && len(s.Reminders) == 0
}

// handleStaleTurns bounds how long an unread result handle is mentioned. Without
// it a handle the model has decided it does not need would be reported for the
// rest of the session.
const handleStaleTurns = 2

// factKey identifies a fact. Two hits on the same line of the same file are one
// fact even when different tools found them.
type factKey struct {
	kind EvidenceKind
	path string
	line int
}

// change is a path the thread wrote and what has since been proved about it.
type change struct {
	turn     uint64
	read     bool
	verified bool
	stale    bool
	// diagnostics is true while a post-edit check reports problems on the path.
	diagnostics bool
}

// read is the last read of a path, used to notice a re-read of content that had
// not changed in between.
type read struct {
	digest string
	turn   uint64
	stale  bool
	// repeatTurn is the turn the path was last re-read unchanged.
	repeatTurn uint64
	repeats    int
}

// handle is a truncated result parked in the content store.
type handle struct {
	tool     string
	turn     uint64
	consumed bool
}

// EvidenceSet accumulates evidence over the turns of one thread.
//
// Facts, changes, reads and handles outlive a turn: a path changed three turns
// ago and still unverified is exactly what the set exists to remember. Call
// counts do not, because repeating a search in a new turn is usually a new
// question.
type EvidenceSet struct {
	mu      sync.Mutex
	facts   map[factKey]EvidenceFact
	changes map[string]*change
	reads   map[string]*read
	handles map[string]*handle

	turn uint64
	// calls counts tool+arguments within the current turn.
	calls map[string]int
	// callTools maps the counted key back to its tool name for the reminder text.
	callTools map[string]string
}

// New returns an empty set.
func NewEvidenceSet() *EvidenceSet {
	return &EvidenceSet{
		facts:     make(map[factKey]EvidenceFact),
		changes:   make(map[string]*change),
		reads:     make(map[string]*read),
		handles:   make(map[string]*handle),
		calls:     make(map[string]int),
		callTools: make(map[string]string),
	}
}

// BeginTurn moves the set to turn and clears the per-turn call counts.
func (s *EvidenceSet) BeginTurn(turn uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = turn
	s.calls = make(map[string]int)
	s.callTools = make(map[string]string)
}

// Observe records a fact. Repeating it keeps the earliest turn: when the thread
// learned something is more useful than when it last saw it again.
func (s *EvidenceSet) Observe(fact EvidenceFact) {
	if s == nil || !fact.Kind.valid() {
		return
	}
	fact.Path = strings.TrimSpace(fact.Path)
	if fact.Path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.facts == nil {
		s.facts = make(map[factKey]EvidenceFact)
	}
	key := factKey{kind: fact.Kind, path: fact.Path, line: fact.Line}
	existing, found := s.facts[key]
	if !found {
		s.facts[key] = fact
		return
	}
	existing.Stale = false
	if fact.Turn < existing.Turn {
		existing.Turn = fact.Turn
	}
	// A later hit may name the symbol an earlier one did not.
	if existing.Symbol == "" && fact.Symbol != "" {
		existing.Symbol = fact.Symbol
	}
	s.facts[key] = existing
}

// MarkChanged records that turn wrote path. read says whether the write rested
// on evidence: a file that was read first, or one the turn created.
func (s *EvidenceSet) MarkChanged(path string, turn uint64, read bool) {
	if s == nil {
		return
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changes == nil {
		s.changes = make(map[string]*change)
	}
	existing, found := s.changes[path]
	if !found {
		s.changes[path] = &change{turn: turn, read: read}
		return
	}
	// A new write invalidates the old verdict: what was verified two turns ago
	// says nothing about the content that just replaced it.
	existing.turn, existing.verified = turn, false
	existing.stale = false
	if read {
		existing.read = true
	}
}

// MarkVerified records that verification covered paths. Only a passing gate may
// call it: a failed run proves the opposite of what it would record.
func (s *EvidenceSet) MarkVerified(paths []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range paths {
		if entry, found := s.changes[strings.TrimSpace(path)]; found {
			entry.verified = true
			entry.stale = false
		}
	}
}

// MarkDiagnostics records whether path's diagnostics are open. A clean run
// clears the flag, so a fixed file stops being reported.
func (s *EvidenceSet) MarkDiagnostics(path string, open bool) {
	if s == nil {
		return
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.changes[path]; found {
		entry.diagnostics = open
	}
}

// NoteCall counts one tool call inside the current turn. Valid JSON arguments
// are canonicalized so whitespace and object key order do not hide repeats.
func (s *EvidenceSet) NoteCall(tool, arguments string) {
	if s == nil || tool == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[string]int)
		s.callTools = make(map[string]string)
	}
	key := tool + "\x00" + canonicalArguments(arguments)
	s.calls[key]++
	s.callTools[key] = tool
}

func canonicalArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	var value any
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return trimmed
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(canonical)
}

// NoteRead records a read of path whose content hashed to digest. A read that
// finds the same digest as the last one produced nothing new.
func (s *EvidenceSet) NoteRead(path, digest string) {
	if s == nil {
		return
	}
	path, digest = strings.TrimSpace(path), strings.TrimSpace(digest)
	if path == "" || digest == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == nil {
		s.reads = make(map[string]*read)
	}
	existing, found := s.reads[path]
	if !found {
		s.reads[path] = &read{digest: digest, turn: s.turn}
		return
	}
	if existing.digest == digest {
		existing.repeats++
		existing.repeatTurn = s.turn
	}
	existing.digest, existing.turn = digest, s.turn
	existing.stale = false
}

// NoteHandle records that a truncated result was parked under handle.
func (s *EvidenceSet) NoteHandle(id, tool string) {
	if s == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handles == nil {
		s.handles = make(map[string]*handle)
	}
	if _, found := s.handles[id]; found {
		return
	}
	s.handles[id] = &handle{tool: tool, turn: s.turn}
}

// ConsumeHandle records that the model retrieved a parked result.
func (s *EvidenceSet) ConsumeHandle(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.handles[strings.TrimSpace(id)]; found {
		entry.consumed = true
	}
}

// EvidenceSnapshot renders the set as of the current turn. limit bounds the facts, which
// are the only part a single grep can inflate; risks and reminders are bounded by
// what the thread changed and called.
func (s *EvidenceSet) Snapshot(limit int) EvidenceSnapshot {
	if s == nil {
		return EvidenceSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	facts := s.selectFacts(limit)
	return EvidenceSnapshot{
		Turn: s.turn, Facts: facts, Risks: s.risks(), Reminders: s.reminders(),
		OmittedFacts: len(s.facts) - len(facts),
	}
}

// selectFacts keeps the most recent facts, then sorts them for presentation. A
// fact from this turn beats one from ten turns ago; among equals the more
// specific kind wins.
func (s *EvidenceSet) selectFacts(limit int) []EvidenceFact {
	facts := make([]EvidenceFact, 0, len(s.facts))
	for _, fact := range s.facts {
		facts = append(facts, fact)
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].Turn != facts[j].Turn {
			return facts[i].Turn > facts[j].Turn
		}
		return lessByKind(facts[i], facts[j])
	})
	if limit > 0 && len(facts) > limit {
		facts = facts[:limit]
	}
	sort.Slice(facts, func(i, j int) bool { return lessByKind(facts[i], facts[j]) })
	return facts
}

func lessByKind(left, right EvidenceFact) bool {
	if left.Kind != right.Kind {
		return left.Kind.rank() < right.Kind.rank()
	}
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	return left.Line < right.Line
}

// risks derives the gaps from the changes. Oldest first: a path that has been
// waiting three turns for a verification is the more pressing one.
func (s *EvidenceSet) risks() []EvidenceRisk {
	var risks []EvidenceRisk
	for path, entry := range s.changes {
		if !entry.verified {
			risks = append(risks, EvidenceRisk{Kind: RiskUnverifiedChange, Path: path, Turn: entry.turn})
		}
		if !entry.read {
			risks = append(risks, EvidenceRisk{Kind: RiskBlindChange, Path: path, Turn: entry.turn})
		}
		if entry.diagnostics {
			risks = append(risks, EvidenceRisk{Kind: RiskOpenDiagnostics, Path: path, Turn: entry.turn})
		}
	}
	sort.Slice(risks, func(i, j int) bool {
		if risks[i].Turn != risks[j].Turn {
			return risks[i].Turn < risks[j].Turn
		}
		if risks[i].Path != risks[j].Path {
			return risks[i].Path < risks[j].Path
		}
		return risks[i].Kind < risks[j].Kind
	})
	return risks
}

// reminders derives the call patterns worth mentioning.
func (s *EvidenceSet) reminders() []EvidenceReminder {
	var reminders []EvidenceReminder
	for key, count := range s.calls {
		if count < 2 {
			continue
		}
		reminders = append(reminders, EvidenceReminder{
			Kind: ReminderRepeatedCall,
			Detail: fmt.Sprintf(
				"%s ran %d times this turn with identical arguments; "+
					"its answer will not change",
				s.callTools[key], count,
			),
		})
	}
	for path, entry := range s.reads {
		// Only a re-read in the current turn is mentioned: an older one is water
		// under the bridge and would crowd out live advice.
		if entry.repeats == 0 || entry.repeatTurn != s.turn {
			continue
		}
		reminders = append(reminders, EvidenceReminder{
			Kind: ReminderRepeatedRead,
			Detail: fmt.Sprintf(
				"%s was read again unchanged; its content is already in this conversation",
				path,
			),
		})
	}
	for id, entry := range s.handles {
		// A handle from the current turn is skipped: the tool result that issued it
		// already carries the notice, and the model has not had its turn yet.
		if entry.consumed || entry.turn >= s.turn || s.turn-entry.turn > handleStaleTurns {
			continue
		}
		reminders = append(reminders, EvidenceReminder{
			Kind: ReminderUnconsumedResult,
			Detail: fmt.Sprintf(
				"%s truncated its result at turn %d and handle %s was never read; "+
					"call result_get if you still need the rest",
				entry.tool, entry.turn, id,
			),
		})
	}
	sort.Slice(reminders, func(i, j int) bool {
		if reminders[i].Kind != reminders[j].Kind {
			return reminders[i].Kind < reminders[j].Kind
		}
		return reminders[i].Detail < reminders[j].Detail
	})
	return reminders
}

// UnverifiedPaths returns the changed paths nothing has verified, sorted. A
// compaction summary carries them: dropping the history is exactly when the
// thread would otherwise forget what it still owes.
func (s *EvidenceSet) UnverifiedPaths() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for path, entry := range s.changes {
		if !entry.verified {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// EvidenceChange is one path the thread wrote, as a caller sees it.
type EvidenceChange struct {
	Path string
	Turn uint64
	// Read is whether the write rested on a read of the same path.
	Read bool
	// Verified is whether a passing verification has covered it since.
	Verified bool
	// Diagnostics is whether a post-edit check still reports problems.
	Diagnostics bool
	// Stale means the workspace no longer matches the content this claim covered.
	Stale bool `json:"stale,omitempty"`
}

// Changes returns every path the thread wrote, sorted, with what has since been
// proved about each.
//
// UnverifiedPaths answers a narrower question — what is still owed — which is
// what the risk list needs. A compaction summary needs the whole picture: a file
// that was verified is still a file the next turn should know it changed, and one
// written without being read stays worth flagging after verification passes.
func (s *EvidenceSet) Changes() []EvidenceChange {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changes := make([]EvidenceChange, 0, len(s.changes))
	for path, entry := range s.changes {
		changes = append(changes, EvidenceChange{
			Path: path, Turn: entry.turn, Read: entry.read,
			Verified: entry.verified, Diagnostics: entry.diagnostics,
			Stale: entry.stale,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// Clone returns an independent copy, so a forked thread inherits what the parent
// proved without sharing its future.
func (s *EvidenceSet) Clone() *EvidenceSet {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := NewEvidenceSet()
	copied.turn = s.turn
	for key, fact := range s.facts {
		copied.facts[key] = fact
	}
	for path, entry := range s.changes {
		clone := *entry
		copied.changes[path] = &clone
	}
	for path, entry := range s.reads {
		clone := *entry
		copied.reads[path] = &clone
	}
	for id, entry := range s.handles {
		clone := *entry
		copied.handles[id] = &clone
	}
	for key, count := range s.calls {
		copied.calls[key] = count
		copied.callTools[key] = s.callTools[key]
	}
	return copied
}
