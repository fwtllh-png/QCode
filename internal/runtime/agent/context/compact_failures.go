package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// Kinds of failure the ledger distinguishes. Two are enough: a tool that errored
// and a verification that did not pass. Both answer the same question for the
// next turn — what has already been tried — but they read differently, because a
// failing command is a fact about the repository while a failing tool call is
// usually a fact about how it was called.
const (
	KindTool   = "tool"
	KindVerify = "verify"
)

// maxFailures bounds the ledger. A thread that fails a hundred distinct ways has
// a problem no summary will fix, and the budget would cut the list anyway.
const maxFailures = 24

// failureReasonBytes caps one reason. Tool errors carry provider or compiler
// output that can run to kilobytes; the ledger keeps the shape. The full log
// stays on the Result Handle and Journal. The removed-history digest keeps
// the tool name, CallID, and the same truncated shape so a compacted model
// can see what already failed.
const failureReasonBytes = 200

// Failure is one attempt that did not work.
type Failure struct {
	// Kind is KindTool or KindVerify.
	Kind string
	// Name is the tool, or the verification scope.
	Name string
	// CallID is the latest tool-call identity for this failure, when known.
	CallID string `json:"call_id,omitempty"`
	// Reason is the error or status as reported.
	Reason string
	// Digest identifies the full failure before display truncation.
	Digest string `json:"Digest,omitempty"`
	// Turn is when it was last seen.
	Turn uint64
	// Count is how many times this exact failure recurred. A repeat is the
	// signal worth surfacing: it says the last correction did not land.
	Count int
}

func (f Failure) line() string {
	var b strings.Builder
	switch f.Kind {
	case KindVerify:
		fmt.Fprintf(&b, "verify %s", f.Name)
	default:
		fmt.Fprintf(&b, "%s", f.Name)
	}
	if f.CallID != "" {
		fmt.Fprintf(&b, " %s", f.CallID)
	}
	if f.Reason != "" {
		fmt.Fprintf(&b, ": %s", f.Reason)
	}
	fmt.Fprintf(&b, " (turn %d", f.Turn)
	if f.Count > 1 {
		fmt.Fprintf(&b, ", %d times", f.Count)
	}
	b.WriteByte(')')
	return b.String()
}

// Failures accumulates the attempts that failed, across turns.
//
// It exists because nothing else keeps them: the receipt's failed-tool list is
// rebuilt every turn, the verify gate's verdict only reaches that turn's event,
// and the diagnostics set is reset. Once a turn's messages are compacted away,
// the fact that a command was already tried and rejected is gone — and a model
// that cannot see its own failed attempts will make them again.
//
// A nil *Failures accepts notes and returns nothing.
type Failures struct {
	mu      sync.Mutex
	records map[string]*Failure
	order   []string
}

// NewFailures returns an empty ledger.
func NewFailures() *Failures {
	return &Failures{records: make(map[string]*Failure)}
}

// NoteTool records that a tool call failed.
func (f *Failures) NoteTool(turn uint64, tool, reason string) {
	f.NoteToolCall(turn, tool, "", reason)
}

// NoteToolCall records a failed tool call together with its CallID.
func (f *Failures) NoteToolCall(turn uint64, tool, callID, reason string) {
	f.note(KindTool, turn, tool, callID, reason)
}

// NoteVerify records that a verification did not pass. scope is what ran,
// status is the verdict, message is whatever detail came with it.
func (f *Failures) NoteVerify(turn uint64, scope, status, message string) {
	reason := strings.TrimSpace(status)
	if detail := strings.TrimSpace(message); detail != "" {
		if reason == "" {
			reason = detail
		} else {
			reason += ": " + detail
		}
	}
	f.note(KindVerify, turn, scope, "", reason)
}

func (f *Failures) note(kind string, turn uint64, name, callID, reason string) {
	if f == nil {
		return
	}
	name = collapse(name)
	if name == "" {
		return
	}
	callID = strings.TrimSpace(callID)
	sum := sha256.Sum256([]byte(reason))
	digest := hex.EncodeToString(sum[:])
	key := kind + "\x00" + name + "\x00" + digest
	reason = truncate(collapse(reason), failureReasonBytes)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = make(map[string]*Failure)
	}
	if existing, found := f.records[key]; found {
		existing.Count++
		existing.Turn = turn
		if callID != "" {
			existing.CallID = callID
		}
		return
	}
	if len(f.order) == maxFailures {
		// Drop the oldest distinct failure rather than refusing the new one: the
		// recent failure is the one the next turn is about to repeat.
		delete(f.records, f.order[0])
		f.order = f.order[1:]
	}
	f.records[key] = &Failure{
		Kind: kind, Name: name, CallID: callID, Reason: reason,
		Digest: digest, Turn: turn, Count: 1,
	}
	f.order = append(f.order, key)
}

// List returns the failures, most recent last seen first.
func (f *Failures) List() []Failure {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Failure, 0, len(f.order))
	for index := len(f.order) - 1; index >= 0; index-- {
		if record, found := f.records[f.order[index]]; found {
			out = append(out, *record)
		}
	}
	return out
}

// Len is how many distinct failures the ledger holds.
func (f *Failures) Len() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

// Clone returns an independent copy, so a forked thread inherits the parent's
// dead ends without sharing its future ones.
func (f *Failures) Clone() *Failures {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := &Failures{
		records: make(map[string]*Failure, len(f.records)),
		order:   make([]string, len(f.order)),
	}
	copy(copied.order, f.order)
	for key, record := range f.records {
		clone := *record
		copied.records[key] = &clone
	}
	return copied
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	if limit <= 3 {
		return text[:limit]
	}
	return TruncateUTF8(text, limit-3) + "..."
}
