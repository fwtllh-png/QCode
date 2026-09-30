// Package trace records where a turn spent its wall clock.
//
// It is deliberately small and local: a recorder collects one turn's spans in
// memory, a sink persists them, and the same tree also answers "how long did
// the model take" without a second set of timers. Measuring a phase twice — once
// for a latency counter and once for a span — is how the two numbers start
// disagreeing.
package trace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

// Status is how a span ended. An open span has no status yet.
type Status string

const (
	StatusOK       Status = "ok"
	StatusError    Status = "error"
	StatusCanceled Status = "canceled"
	// StatusOpen marks a span the turn never closed, which is what a crash or a
	// dropped error path leaves behind. It is recorded rather than repaired: a
	// span silently given "ok" would claim the phase finished.
	StatusOpen Status = "open"
)

// Span names. They are a closed set on purpose: a query like "how much of this
// turn was spent waiting for a human" has to match on something stable.
const (
	// NameTurn is the root span every turn opens.
	NameTurn = "turn"
	// NameModelCall is one provider call, which is one usage sample. A retry is
	// its own call and its own span, because a retried call really did spend the
	// time and the tokens.
	NameModelCall = "model_call"
	// NameTool is one tool invocation, from admission to result.
	NameTool = "tool"
	// NameApprovalWait is the stretch a tool spent parked waiting for a human. It
	// is a child of the tool span because the wait happens inside the call.
	NameApprovalWait = "approval_wait"
	// NameVerify is one verification gate evaluation.
	NameVerify = "verify"
	// NameTurnKernelTransition records one Coordinator-committed transition.
	NameTurnKernelTransition = "turn_kernel_transition"
	// NameCleanup records work after the immutable terminal measurement was
	// frozen. It is excluded from Turn latency and Receipt accounting.
	NameCleanup = "turn_cleanup"
)

// Record is one span as a reader sees it: the recorder's own state is not
// exposed, so a snapshot can be handed to a sink without it changing underfoot.
type Record struct {
	ID       uint64
	ParentID uint64
	Name     string
	Started  time.Time
	// Ended is zero while the span is still open.
	Ended      time.Time
	Status     Status
	Attributes map[string]any
}

// Open reports whether the span never ended.
func (r Record) Open() bool { return r.Ended.IsZero() }

// Duration is how long the span ran. An open span has no duration: the caller
// that wants "so far" has to say what "now" is, which only the recorder knows.
func (r Record) Duration() time.Duration {
	if r.Open() {
		return 0
	}
	return r.Ended.Sub(r.Started)
}

// Latency is the span tree summarized for the turn receipt.
//
// Each phase is a sum over its spans, so the phases do not partition Total and
// adding them up means nothing:
//
//	ApprovalWait ⊆ Tool     a tool parks for approval inside its own call
//	Tool         ⋛ Total    tools run in parallel, so their sum can exceed the
//	                        wall clock the turn actually took
//	Provider     ⋛ Total    the turn's own calls are sequential, but a tool that
//	                        samples a model (vision, sub_query) adds a model call
//	                        inside a tool span — so provider time overlaps tool
//	                        time, and parallel tools can push the sum past the
//	                        wall clock too
//
// A zero here means the phase was measured and cost nothing — no tool ran, no
// human was asked. It never means "we did not look": a caller that measured
// nothing at all has no Latency to report in the first place. FirstToken is the
// one exception and therefore the one pointer: a turn whose model never emitted
// anything has no honest zero to report, because zero would read as "the first
// token arrived instantly".
type Latency struct {
	Total        time.Duration
	FirstToken   *time.Duration
	Provider     time.Duration
	Tool         time.Duration
	ApprovalWait time.Duration
	Verify       time.Duration
}

type FrozenMeasurement struct {
	FrozenAt time.Time
	Latency  Latency
	Recorded bool
}

// Recorder collects one turn's spans. It is safe for concurrent use because the
// tool phase runs several tools at once.
type Recorder struct {
	mu               sync.Mutex
	now              func() time.Time
	next             uint64
	spans            []*Record
	byID             map[uint64]*Record
	spanIDs          map[uint64]string
	traceID          string
	traceState       string
	traceFlags       byte
	remoteParentSpan string
	root             uint64
	firstToken       time.Time
	frozen           FrozenMeasurement
	cleanup          bool
	onClose          func()
	closed           bool
}

var fallbackTraceID atomic.Uint64

// NewRecorder returns a recorder that reads time from now. A nil clock means
// time.Now, and tests pass their own.
func NewRecorder(now func() time.Time) *Recorder {
	return NewRecorderContext(context.Background(), now)
}

func NewRecorderContext(
	ctx context.Context,
	now func() time.Time,
) *Recorder {
	if now == nil {
		now = time.Now
	}
	recorder := &Recorder{
		now: now, byID: make(map[uint64]*Record),
		spanIDs: make(map[uint64]string),
	}
	if parent, ok := tracecontext.Current(ctx); ok {
		recorder.traceID = parent.TraceID
		recorder.traceState = parent.TraceState
		recorder.traceFlags = parent.TraceFlags
		recorder.remoteParentSpan = parent.SpanID
	} else {
		recorder.traceID = newTraceHex(16)
		recorder.traceFlags = 1
	}
	return recorder
}

// Span is a handle to an open span. Every method tolerates a nil handle so a
// caller does not have to guard each call site.
type Span struct {
	recorder *Recorder
	id       uint64
}

// ID is the span's identifier within the turn, which is what a child span
// names as its parent. Zero means no span.
func (s *Span) ID() uint64 {
	if s == nil {
		return 0
	}
	return s.id
}

// Start opens a span. A parent of zero attaches the span to the turn root, or
// makes it the root if the turn has none yet.
func (r *Recorder) Start(name string, parent uint64, attributes map[string]any) *Span {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.next++
	if parent == 0 {
		parent = r.root
	}
	record := &Record{
		ID: r.next, ParentID: parent, Name: name,
		Started: r.now(), Status: StatusOpen, Attributes: copyAttributes(attributes),
	}
	if r.root == 0 {
		r.root = record.ID
		record.ParentID = 0
	}
	r.spans = append(r.spans, record)
	r.byID[record.ID] = record
	r.spanIDs[record.ID] = newTraceHex(8)
	r.mu.Unlock()
	return &Span{recorder: r, id: record.ID}
}

// Add records a span whose bounds were measured somewhere else, which is how a
// wait that starts and ends inside another component is recorded: the component
// reports how long it waited, and the recorder places that stretch in the tree
// rather than re-measuring it from the outside and including work that is not
// waiting.
func (r *Recorder) Add(
	name string,
	parent uint64,
	started, ended time.Time,
	status Status,
	attributes map[string]any,
) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.next++
	if parent == 0 {
		parent = r.root
	}
	record := &Record{
		ID: r.next, ParentID: parent, Name: name, Started: started, Ended: ended,
		Status: status, Attributes: copyAttributes(attributes),
	}
	if r.root == 0 {
		r.root = record.ID
		record.ParentID = 0
	}
	r.spans = append(r.spans, record)
	r.byID[record.ID] = record
	r.spanIDs[record.ID] = newTraceHex(8)
	r.mu.Unlock()
}

// End closes the span. Ending twice keeps the first ending: the first one is
// when the work actually stopped.
func (s *Span) End(status Status) {
	if s == nil || s.recorder == nil {
		return
	}
	recorder := s.recorder
	recorder.mu.Lock()
	record := recorder.byID[s.id]
	if record == nil || !record.Ended.IsZero() {
		recorder.mu.Unlock()
		return
	}
	record.Ended, record.Status = recorder.now(), status
	recorder.mu.Unlock()
}

// Set attaches an attribute to the span.
func (s *Span) Set(key string, value any) {
	if s == nil || s.recorder == nil || key == "" {
		return
	}
	recorder := s.recorder
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	record := recorder.byID[s.id]
	if record == nil || !record.Ended.IsZero() {
		return
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]any, 1)
	}
	record.Attributes[key] = value
}

// NoteFirstOutput stamps when the model first produced something a user could
// see. Only the first call counts; later output is not a first token.
func (r *Recorder) NoteFirstOutput() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstToken.IsZero() {
		r.firstToken = r.now()
	}
}

// Spans is a snapshot of the turn's spans in the order they opened.
func (r *Recorder) Spans() []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]Record, 0, len(r.spans))
	for _, record := range r.spans {
		snapshot := *record
		snapshot.Attributes = copyAttributes(record.Attributes)
		records = append(records, snapshot)
	}
	return records
}

func (r *Recorder) Context(
	ctx context.Context,
	span uint64,
) context.Context {
	if r == nil || span == 0 {
		return ctx
	}
	r.mu.Lock()
	link := tracecontext.Link{
		TraceID:    r.traceID,
		SpanID:     r.spanIDs[span],
		TraceState: r.traceState,
		TraceFlags: r.traceFlags,
	}
	r.mu.Unlock()
	if link.TraceID == "" || link.SpanID == "" {
		return ctx
	}
	current, err := tracecontext.WithLink(ctx, link, false)
	if err != nil {
		return ctx
	}
	return current
}

// Latency summarizes the tree. It is safe to call while the turn is still
// running, which is what the receipt does: the receipt is emitted before the
// terminal event, so the root span is still open and Total is "so far".
func (r *Recorder) Latency() Latency {
	if r == nil {
		return Latency{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen.Recorded {
		return r.frozen.Latency
	}
	now := r.now()
	return r.latencyLocked(now)
}

func (r *Recorder) latencyLocked(now time.Time) Latency {
	latency := Latency{}
	if root := r.byID[r.root]; root != nil {
		latency.Total = r.elapsed(root, now)
		if !r.firstToken.IsZero() {
			first := r.firstToken.Sub(root.Started)
			latency.FirstToken = &first
		}
	}
	for _, record := range r.spans {
		switch record.Name {
		case NameModelCall:
			latency.Provider += r.elapsed(record, now)
		case NameTool:
			latency.Tool += r.elapsed(record, now)
		case NameApprovalWait:
			latency.ApprovalWait += r.elapsed(record, now)
		case NameVerify:
			latency.Verify += r.elapsed(record, now)
		}
	}
	return latency
}

func (r *Recorder) FreezeTerminal(status Status) FrozenMeasurement {
	if r == nil {
		return FrozenMeasurement{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen.Recorded {
		return r.frozen
	}
	now := r.now()
	root := r.byID[r.root]
	if root == nil {
		return FrozenMeasurement{}
	}
	if root.Ended.IsZero() {
		root.Ended = now
		root.Status = status
	}
	for _, record := range r.spans {
		if record.Ended.IsZero() {
			record.Ended = now
		}
	}
	r.frozen = FrozenMeasurement{
		FrozenAt: now.UTC(),
		Latency:  r.latencyLocked(now),
		Recorded: true,
	}
	return r.frozen
}

func (r *Recorder) TerminalFrozen() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frozen.Recorded
}

func (r *Recorder) CloseWithCleanup(status Status) []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.frozen.Recorded && !r.cleanup {
		now := r.now()
		if now.Before(r.frozen.FrozenAt) {
			now = r.frozen.FrozenAt
		}
		r.next++
		r.spans = append(r.spans, &Record{
			ID: r.next, ParentID: r.root, Name: NameCleanup,
			Started: r.frozen.FrozenAt, Ended: now,
			Status: status,
			Attributes: map[string]any{
				"excluded_from_terminal_measurement": true,
			},
		})
		r.cleanup = true
	}
	r.mu.Unlock()
	return r.Close()
}

// Close ends every span the turn left open and returns the snapshot. A turn
// that failed halfway leaves open spans, and reporting them as open is more
// useful than pretending they were never started.
func (r *Recorder) Close() []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	now := r.now()
	for _, record := range r.spans {
		if record.Ended.IsZero() {
			record.Ended = now
		}
	}
	onClose := r.onClose
	if r.closed {
		onClose = nil
	} else {
		r.closed = true
	}
	r.mu.Unlock()
	if onClose != nil {
		onClose()
	}
	return r.Spans()
}

// elapsed is how long a span has run so far. The caller holds the lock.
func (r *Recorder) elapsed(record *Record, now time.Time) time.Duration {
	if record.Ended.IsZero() {
		return now.Sub(record.Started)
	}
	return record.Ended.Sub(record.Started)
}

func copyAttributes(attributes map[string]any) map[string]any {
	if len(attributes) == 0 {
		return nil
	}
	copied := make(map[string]any, len(attributes))
	for key, value := range attributes {
		copied[key] = value
	}
	return copied
}

func newTraceHex(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	seed := fmt.Sprintf("%d:%d", time.Now().UnixNano(), fallbackTraceID.Add(1))
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:bytes])
}
