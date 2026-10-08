package workspacejournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

var (
	ErrUnread        = errors.New("read-before-edit required")
	ErrStale         = errors.New("file changed since last successful read")
	ErrRetainedDraft = errors.New("workspace journal has a retained draft")

	errWorkspaceRestoreInProgress = errors.New("workspace turn is already being restored")
)

// ReadValidationError identifies the exact workspace-relative path whose
// read fingerprint is missing or stale. Callers can use the structured path
// for recovery without parsing the human-readable error string.
type ReadValidationError struct {
	Path  string
	Cause error
}

func (e *ReadValidationError) Error() string {
	return fmt.Sprintf("read validation %q: %v", e.Path, e.Cause)
}

func (e *ReadValidationError) Unwrap() error {
	return e.Cause
}

type Identity struct {
	Device      uint64 `json:"device,omitempty"`
	Inode       uint64 `json:"inode,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	ModTimeNano int64  `json:"mtime_ns"`
	Size        int64  `json:"size"`
}

type Fingerprint struct {
	Path     string   `json:"path"`
	SHA256   string   `json:"sha256"`
	Identity Identity `json:"identity"`
	Exists   bool     `json:"exists"`
}

func Snapshot(path string) (Fingerprint, []byte, fs.FileMode, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Fingerprint{Path: filepath.Clean(path)}, nil, 0, nil
	}
	if err != nil {
		return Fingerprint{}, nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Fingerprint{}, nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return Fingerprint{}, nil, 0, errors.New("workspace fingerprint target is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return Fingerprint{}, nil, 0, err
	}
	sum := sha256.Sum256(data)
	return Fingerprint{
		Path: filepath.Clean(path), SHA256: hex.EncodeToString(sum[:]),
		Identity: identity(info), Exists: true,
	}, data, info.Mode().Perm(), nil
}

func identity(info fs.FileInfo) Identity {
	result := Identity{
		ModTimeNano: info.ModTime().UnixNano(),
		Size:        info.Size(),
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		result.Device = unsignedField(value, "Dev")
		result.Inode = unsignedField(value, "Ino")
		high := unsignedField(value, "FileIndexHigh")
		low := unsignedField(value, "FileIndexLow")
		if high != 0 || low != 0 {
			result.FileID = fmt.Sprintf("%016x", high<<32|low)
		}
	}
	if result.FileID == "" && result.Inode != 0 {
		result.FileID = fmt.Sprintf("%x:%x", result.Device, result.Inode)
	}
	return result
}

func unsignedField(value reflect.Value, name string) uint64 {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(field.Int())
	default:
		return 0
	}
}

func linkCount(info fs.FileInfo) uint64 {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0
	}
	if links := unsignedField(value, "Nlink"); links != 0 {
		return links
	}
	return unsignedField(value, "NumberOfLinks")
}

func Equal(left, right Fingerprint) bool {
	if left.Path != right.Path || left.Exists != right.Exists {
		return false
	}
	if !left.Exists {
		return true
	}
	return left.SHA256 == right.SHA256 &&
		left.Identity.Device == right.Identity.Device &&
		left.Identity.Inode == right.Identity.Inode &&
		left.Identity.FileID == right.Identity.FileID &&
		left.Identity.ModTimeNano == right.Identity.ModTimeNano &&
		left.Identity.Size == right.Identity.Size
}

type ReadTracker struct {
	mu    sync.Mutex
	reads map[string]Fingerprint
}

type expectedWritesKey struct{}

func WithExpectedWrites(ctx context.Context, values map[string]Fingerprint) context.Context {
	copy := make(map[string]Fingerprint, len(values))
	for path, fingerprint := range values {
		copy[filepath.Clean(path)] = fingerprint
	}
	return context.WithValue(ctx, expectedWritesKey{}, copy)
}

func ValidateExpectedWrite(ctx context.Context, path string) error {
	values, _ := ctx.Value(expectedWritesKey{}).(map[string]Fingerprint)
	if len(values) == 0 {
		return nil
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return err
	}
	path = canonical
	expected, exists := values[path]
	if !exists {
		return fmt.Errorf("expected write fingerprint for %q is missing", path)
	}
	current, _, _, err := Snapshot(path)
	if err != nil {
		return err
	}
	if !Equal(expected, current) {
		return ErrStale
	}
	return nil
}

func NewReadTracker() *ReadTracker {
	return &ReadTracker{reads: make(map[string]Fingerprint)}
}

func (t *ReadTracker) Record(path string) (Fingerprint, error) {
	fingerprint, _, _, err := Snapshot(path)
	if err != nil {
		return Fingerprint{}, err
	}
	if !fingerprint.Exists {
		return Fingerprint{}, os.ErrNotExist
	}
	t.mu.Lock()
	t.reads[fingerprint.Path] = fingerprint
	t.mu.Unlock()
	return fingerprint, nil
}

func (t *ReadTracker) RecordFingerprint(fingerprint Fingerprint) error {
	if !fingerprint.Exists {
		t.Invalidate(fingerprint.Path)
		return nil
	}
	current, _, _, err := Snapshot(fingerprint.Path)
	if err != nil {
		return err
	}
	if !Equal(fingerprint, current) {
		t.Invalidate(fingerprint.Path)
		return ErrStale
	}
	t.mu.Lock()
	t.reads[current.Path] = current
	t.mu.Unlock()
	return nil
}

func (t *ReadTracker) ValidateWrite(path string) (Fingerprint, error) {
	path = filepath.Clean(path)
	current, _, _, err := Snapshot(path)
	if err != nil {
		return Fingerprint{}, err
	}
	t.mu.Lock()
	read, readExists := t.reads[path]
	t.mu.Unlock()
	if !current.Exists {
		if readExists {
			return Fingerprint{}, ErrStale
		}
		return current, nil
	}
	if !readExists {
		return Fingerprint{}, ErrUnread
	}
	if !Equal(read, current) {
		return Fingerprint{}, ErrStale
	}
	return current, nil
}

func (t *ReadTracker) Invalidate(path string) {
	t.mu.Lock()
	delete(t.reads, filepath.Clean(path))
	t.mu.Unlock()
}

type Record struct {
	Path         string      `json:"path"`
	Before       Fingerprint `json:"before"`
	BeforeHandle string      `json:"before_handle,omitempty"`
	BeforeMode   fs.FileMode `json:"before_mode,omitempty"`
	After        Fingerprint `json:"after"`
}

// Change kinds. This is the vocabulary for "what happened to a path" across the
// runtime, so the guard, the receipt and the journal all agree.
const (
	ChangeCreated  = "created"
	ChangeModified = "modified"
	ChangeDeleted  = "deleted"
)

// Kind classifies a record by content: rewriting a file with identical bytes is
// not a change even though its identity (mtime/inode) moved. An empty kind means
// the path ended the turn as it started.
func (r Record) Kind() string {
	switch {
	case !r.Before.Exists && r.After.Exists:
		return ChangeCreated
	case r.Before.Exists && !r.After.Exists:
		return ChangeDeleted
	case r.Before.Exists && r.After.Exists && r.Before.SHA256 != r.After.SHA256:
		return ChangeModified
	default:
		return ""
	}
}

// Change is one path the active turn changed, with the fingerprint of both
// sides. Content is fetched separately via BeforeImage so listing stays cheap.
type Change struct {
	Path   string      `json:"path"`
	Kind   string      `json:"kind"`
	Before Fingerprint `json:"before"`
	After  Fingerprint `json:"after"`
}

type turnJournal struct {
	id      string
	order   []string
	records map[string]*Record
	started time.Time
	// restoring is guarded by Manager.mu. While set, a Rollback or Revert
	// owns the journal and no other operation may resume, keep, or restore it.
	restoring bool
}

type Conflict struct {
	Path     string      `json:"path"`
	Reason   string      `json:"reason"`
	Expected Fingerprint `json:"expected"`
	Current  Fingerprint `json:"current"`
}

type Receipt struct {
	TurnID                     string     `json:"turn_id"`
	Restored                   []string   `json:"restored"`
	Conflicts                  []Conflict `json:"conflicts"`
	NonFileSideEffectsReverted bool       `json:"non_file_side_effects_reverted"`
	NonFileSideEffectsNote     string     `json:"non_file_side_effects_note"`
}

type Manager struct {
	mu        sync.Mutex
	root      string
	workspace *sandbox.Workspace
	store     contentstore.Store
	active    *turnJournal
	committed map[string]*turnJournal
	drafts    map[string]*turnJournal
	// unresolved keeps journals whose rollback hit a conflict, so their
	// before-images survive for a retry.
	unresolved map[string]*turnJournal
	restoreMu  sync.Mutex
	// ledger is nil for an in-memory journal. When set, every record reaches disk
	// before the write it describes, so another process can undo an interrupted
	// turn.
	ledger *ledger
	owner  string
	pid    int
}

func New(root string, store contentstore.Store) (*Manager, error) {
	if store == nil {
		return nil, errors.New("workspace journal content store is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	workspace, err := sandbox.NewWorkspace(canonical)
	if err != nil {
		return nil, err
	}
	return &Manager{
		root: workspace.Root(), workspace: workspace, store: store,
		committed:  make(map[string]*turnJournal),
		drafts:     make(map[string]*turnJournal),
		unresolved: make(map[string]*turnJournal),
	}, nil
}

func (m *Manager) Begin(turnID string) error {
	if turnID == "" {
		return errors.New("workspace journal turn id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return errors.New("workspace journal already has an active turn")
	}
	if len(m.drafts) != 0 {
		return fmt.Errorf(
			"%w; continue, retry, or revert it first",
			ErrRetainedDraft,
		)
	}
	started := time.Now().UTC()
	if err := m.ledger.append(entry{
		Phase: phaseBegin, TurnID: turnID, Owner: m.owner, PID: m.pid, At: started,
	}); err != nil {
		return err
	}
	m.active = &turnJournal{
		id: turnID, records: make(map[string]*Record), started: started,
	}
	return nil
}

func (m *Manager) Before(ctx context.Context, path string) error {
	var err error
	path, err = canonicalPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.active == nil {
		m.mu.Unlock()
		return errors.New("workspace journal has no active turn")
	}
	if m.active.records[path] != nil {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	before, data, mode, err := Snapshot(path)
	if err != nil {
		return err
	}
	handle := ""
	if before.Exists {
		handle = contentstore.StableHandle("workspace", data)
		if err := m.store.Put(ctx, handle, data); err != nil {
			return fmt.Errorf("store workspace before-image: %w", err)
		}
	}
	record := &Record{
		Path: path, Before: before, BeforeHandle: handle, BeforeMode: mode, After: before,
	}
	m.mu.Lock()
	if m.active != nil && m.active.records[path] == nil {
		// The record must be on disk before the tool writes, or a crash between the
		// two leaves a changed file with no before-image to restore.
		durableRecord, err := m.durableRecord(record)
		if err != nil {
			m.mu.Unlock()
			if handle != "" {
				_ = m.store.Release(context.Background(), handle)
			}
			return err
		}
		if err := m.ledger.append(entry{
			Phase: phaseBefore, TurnID: m.active.id, Record: durableRecord,
		}); err != nil {
			m.mu.Unlock()
			if handle != "" {
				_ = m.store.Release(context.Background(), handle)
			}
			return err
		}
	}
	defer m.mu.Unlock()
	if m.active == nil {
		if handle != "" {
			_ = m.store.Release(context.Background(), handle)
		}
		return errors.New("workspace journal turn ended while recording before-image")
	}
	if m.active.records[path] == nil {
		m.active.records[path] = record
		m.active.order = append(m.active.order, path)
	} else if handle != "" {
		_ = m.store.Release(context.Background(), handle)
	}
	return nil
}

func (m *Manager) After(path string) error {
	_, err := m.AfterFingerprint(path)
	return err
}

func (m *Manager) AfterFingerprint(path string) (Fingerprint, error) {
	var err error
	path, err = canonicalPath(path)
	if err != nil {
		return Fingerprint{}, err
	}
	after, _, _, err := Snapshot(path)
	if err != nil {
		return Fingerprint{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return Fingerprint{}, errors.New("workspace journal has no active turn")
	}
	record := m.active.records[path]
	if record == nil {
		return Fingerprint{}, errors.New("workspace journal before-image is missing")
	}
	record.After = after
	// Rollback compares the file on disk against this fingerprint, so a recovering
	// process needs it as much as this one does.
	durableRecord, err := m.durableRecord(record)
	if err != nil {
		return Fingerprint{}, err
	}
	if err := m.ledger.append(entry{
		Phase: phaseAfter, TurnID: m.active.id, Record: durableRecord,
	}); err != nil {
		return Fingerprint{}, err
	}
	return after, nil
}

func (m *Manager) durableRecord(record *Record) (*Record, error) {
	if record == nil {
		return nil, errors.New("workspace journal record is required")
	}
	relative, err := filepath.Rel(m.root, record.Path)
	if err != nil || relative == "." || relative == ".." ||
		filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("workspace journal record path escapes root")
	}
	relative = filepath.Clean(relative)
	copy := *record
	copy.Path = relative
	copy.Before.Path = relative
	copy.After.Path = relative
	return &copy, nil
}

// Changes lists the paths the active turn has changed so far, in write order.
// Paths the turn touched without changing their content are left out.
func (m *Manager) Changes() []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	changes := make([]Change, 0, len(m.active.order))
	for _, path := range m.active.order {
		record := m.active.records[path]
		kind := record.Kind()
		if kind == "" {
			continue
		}
		changes = append(changes, Change{
			Path: record.Path, Kind: kind, Before: record.Before, After: record.After,
		})
	}
	return changes
}

// ObservedChanges returns every journaled path, including paths restored to
// the turn baseline (Kind is empty). The identity check prevents an absent or
// unrelated journal from being interpreted as evidence of no changes.
func (m *Manager) ObservedChanges(turnID string) ([]Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.id != turnID {
		return nil, errors.New("workspace journal active turn does not match observation")
	}
	changes := make([]Change, 0, len(m.active.order))
	for _, path := range m.active.order {
		record := m.active.records[path]
		changes = append(changes, Change{Path: record.Path, Kind: record.Kind(), Before: record.Before, After: record.After})
	}
	return changes, nil
}

// BeforeImage returns the bytes recorded before the active turn first wrote
// path. found is false when the turn has no record for the path, and existed is
// false when the path did not exist before the turn.
func (m *Manager) BeforeImage(
	ctx context.Context, path string,
) (data []byte, existed, found bool, err error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, false, false, err
	}
	m.mu.Lock()
	var record Record
	if m.active != nil {
		if active := m.active.records[canonical]; active != nil {
			record, found = *active, true
		}
	}
	m.mu.Unlock()
	switch {
	case !found:
		return nil, false, false, nil
	case !record.Before.Exists:
		return nil, false, true, nil
	}
	data, err = m.store.Get(ctx, record.BeforeHandle)
	if err != nil {
		return nil, true, true, fmt.Errorf("load workspace before-image: %w", err)
	}
	return data, true, true, nil
}

func canonicalPath(path string) (string, error) {
	current := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func (m *Manager) Commit(turnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.committed[turnID] != nil {
		return nil
	}
	if m.active == nil || m.active.id != turnID {
		return errors.New("workspace journal active turn does not match finalization")
	}
	if err := m.ledger.append(entry{
		Phase: phaseCommit, TurnID: turnID,
	}); err != nil {
		return err
	}
	journal, err := m.finishActiveLocked(turnID)
	if err != nil {
		return err
	}
	m.committed[turnID] = journal
	// A committed turn is no longer something a later process should undo: it
	// passed its gate, so its changes are finished work.
	return nil
}

// Suspend preserves an unverified Turn as a resumable draft. Unlike Commit, it
// keeps the before-images durable across process restart.
func (m *Manager) Suspend(turnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[turnID] != nil {
		return nil
	}
	if m.active == nil || m.active.id != turnID {
		return errors.New("workspace journal active turn does not match suspension")
	}
	if !journalHasChanges(m.active) {
		if err := m.ledger.append(entry{
			Phase: phaseSettled, TurnID: turnID,
		}); err != nil {
			return err
		}
		_, err := m.finishActiveLocked(turnID)
		return err
	}
	if err := m.ledger.append(entry{
		Phase: phaseDraft, TurnID: turnID,
	}); err != nil {
		return err
	}
	journal := m.active
	m.active = nil
	m.drafts[turnID] = journal
	return nil
}

func journalHasChanges(journal *turnJournal) bool {
	if journal == nil {
		return false
	}
	for _, path := range journal.order {
		if record := journal.records[path]; record != nil && record.Kind() != "" {
			return true
		}
	}
	return false
}

// HasDraft reports whether a terminal Turn retained a resumable workspace
// draft in this journal.
func (m *Manager) HasDraft(turnID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drafts[turnID] != nil
}

// KeepDraft settles a user's explicit context withdrawal without changing files.
// This is not verification: callers must retain unverified change evidence.
func (m *Manager) KeepDraft(turnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal := m.drafts[turnID]
	if journal == nil {
		return nil
	}
	if journal.restoring {
		return errWorkspaceRestoreInProgress
	}
	if err := m.ledger.append(entry{Phase: phaseCommit, TurnID: turnID}); err != nil {
		return err
	}
	delete(m.drafts, turnID)
	m.committed[turnID] = journal
	return nil
}

// DraftTurnIDs lists terminal Turns that still retain a workspace draft.
func (m *Manager) DraftTurnIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.drafts))
	for turnID := range m.drafts {
		ids = append(ids, turnID)
	}
	sort.Strings(ids)
	return ids
}

// DraftChanges returns the retained net changes for a terminal draft.
func (m *Manager) DraftChanges(turnID string) []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal := m.drafts[turnID]
	if journal == nil {
		return nil
	}
	changes := make([]Change, 0, len(journal.order))
	for _, path := range journal.order {
		record := journal.records[path]
		if record == nil || record.Kind() == "" {
			continue
		}
		changes = append(changes, Change{
			Path: record.Path, Kind: record.Kind(),
			Before: record.Before, After: record.After,
		})
	}
	return changes
}

// ResumeDraft atomically rebinds a retained draft to the new recovery Turn.
// Keeping one journal preserves the original before-images and avoids a chain
// of partial baselines across repeated Continue actions.
func (m *Manager) ResumeDraft(sourceTurnID, recoveryTurnID string) error {
	if sourceTurnID == "" || recoveryTurnID == "" {
		return errors.New("workspace draft recovery identity is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return errors.New("workspace journal already has an active turn")
	}
	journal := m.drafts[sourceTurnID]
	if journal == nil {
		return errors.New("workspace draft was not found")
	}
	if journal.restoring {
		return errWorkspaceRestoreInProgress
	}
	if sourceTurnID == recoveryTurnID {
		delete(m.drafts, sourceTurnID)
		m.active = journal
		return nil
	}
	if err := m.ledger.append(entry{
		Phase: phaseResume, TurnID: recoveryTurnID,
		SourceTurnID: sourceTurnID, Owner: m.owner, PID: m.pid,
	}); err != nil {
		return err
	}
	delete(m.drafts, sourceTurnID)
	journal.id = recoveryTurnID
	m.active = journal
	return nil
}

func (m *Manager) finishActiveLocked(turnID string) (*turnJournal, error) {
	if m.active == nil || m.active.id != turnID {
		return nil, errors.New("workspace journal active turn does not match finalization")
	}
	journal := m.active
	m.active = nil
	filtered := journal.order[:0]
	for _, path := range journal.order {
		record := journal.records[path]
		if Equal(record.Before, record.After) {
			if record.BeforeHandle != "" {
				_ = m.store.Release(context.Background(), record.BeforeHandle)
			}
			delete(journal.records, path)
			continue
		}
		filtered = append(filtered, path)
	}
	journal.order = filtered
	return journal, nil
}

// Rollback undoes the active turn. A conflict leaves the remaining before-images
// in place and keeps the journal addressable, so calling Rollback again after
// the conflict is dealt with retries only the paths that were not restored.
func (m *Manager) Rollback(ctx context.Context, turnID string) (Receipt, error) {
	m.mu.Lock()
	journal := m.active
	if journal != nil && journal.id == turnID {
		m.active = nil
	} else {
		journal = m.unresolved[turnID]
	}
	if journal == nil {
		m.mu.Unlock()
		return Receipt{}, errors.New("workspace journal active turn does not match rollback")
	}
	if journal.restoring {
		m.mu.Unlock()
		return Receipt{}, errWorkspaceRestoreInProgress
	}
	journal.restoring = true
	m.mu.Unlock()
	receipt, err := m.restore(ctx, journal)
	resolved := err == nil && len(receipt.Conflicts) == 0
	m.mu.Lock()
	journal.restoring = false
	if !resolved {
		m.unresolved[turnID] = journal
	}
	m.mu.Unlock()
	if resolved {
		if settleErr := m.ledger.append(entry{
			Phase: phaseSettled, TurnID: turnID,
		}); settleErr != nil {
			m.mu.Lock()
			m.unresolved[turnID] = journal
			m.mu.Unlock()
			return receipt, settleErr
		}
		m.mu.Lock()
		delete(m.unresolved, turnID)
		m.mu.Unlock()
		m.release(journal)
	}
	return receipt, err
}

func (m *Manager) Revert(ctx context.Context, turnID string) (Receipt, error) {
	m.mu.Lock()
	journal := m.committed[turnID]
	draft := false
	if journal == nil {
		journal = m.drafts[turnID]
		draft = journal != nil
	}
	if journal == nil {
		m.mu.Unlock()
		return Receipt{}, errors.New("committed workspace turn or draft was not found")
	}
	if journal.restoring {
		m.mu.Unlock()
		return Receipt{}, errWorkspaceRestoreInProgress
	}
	journal.restoring = true
	m.mu.Unlock()
	receipt, err := m.restore(ctx, journal)
	resolved := err == nil && len(receipt.Conflicts) == 0
	m.mu.Lock()
	journal.restoring = false
	if resolved {
		if draft {
			delete(m.drafts, turnID)
		} else {
			delete(m.committed, turnID)
		}
	}
	m.mu.Unlock()
	if resolved {
		m.release(journal)
		if settleErr := m.ledger.append(entry{
			Phase: phaseSettled, TurnID: turnID,
		}); settleErr != nil {
			return receipt, settleErr
		}
	}
	return receipt, err
}

// restore requires the caller to hold the journal's restoring claim. It prunes
// restored records under mu so concurrent readers of the journal stay safe.
func (m *Manager) restore(ctx context.Context, journal *turnJournal) (Receipt, error) {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	m.mu.Lock()
	turnID := journal.id
	records := make([]*Record, 0, len(journal.order))
	for _, path := range journal.order {
		records = append(records, journal.records[path])
	}
	m.mu.Unlock()
	receipt := Receipt{
		TurnID: turnID, NonFileSideEffectsReverted: false,
		NonFileSideEffectsNote: "only declared file resources are reverted; process, network, and other side effects are not rolled back",
	}
	for index := len(records) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		record := records[index]
		current, _, _, err := Snapshot(record.Path)
		if err != nil {
			receipt.Conflicts = append(receipt.Conflicts, Conflict{
				Path: record.Path, Reason: err.Error(), Expected: record.After,
			})
			continue
		}
		if !Equal(current, record.After) {
			receipt.Conflicts = append(receipt.Conflicts, Conflict{
				Path: record.Path, Reason: "workspace file changed after the recorded write",
				Expected: record.After, Current: current,
			})
			continue
		}
		if err := m.restoreRecord(ctx, record); err != nil {
			receipt.Conflicts = append(receipt.Conflicts, Conflict{
				Path: record.Path, Reason: err.Error(), Expected: record.After, Current: current,
			})
			continue
		}
		receipt.Restored = append(receipt.Restored, record.Path)
		// Drop what is already back in place: a retry after a conflict must not
		// try to restore it a second time, which would now itself conflict.
		m.forget(journal, record)
	}
	sort.Strings(receipt.Restored)
	sort.Slice(receipt.Conflicts, func(i, j int) bool {
		return receipt.Conflicts[i].Path < receipt.Conflicts[j].Path
	})
	if len(receipt.Conflicts) != 0 {
		return receipt, fmt.Errorf("workspace rollback has %d conflict(s)", len(receipt.Conflicts))
	}
	return receipt, nil
}

func (m *Manager) restoreRecord(ctx context.Context, record *Record) error {
	relative, err := filepath.Rel(m.root, record.Path)
	if err != nil || relative == "." || relative == ".." ||
		filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("workspace restore path escapes root")
	}
	if !record.Before.Exists {
		if err := m.workspace.Remove(relative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := m.store.Get(ctx, record.BeforeHandle)
	if err != nil {
		return fmt.Errorf("load workspace before-image: %w", err)
	}
	return m.workspace.AtomicWrite(relative, data, record.BeforeMode)
}

// forget drops a restored record and releases its before-image.
func (m *Manager) forget(journal *turnJournal, record *Record) {
	m.mu.Lock()
	handle := record.BeforeHandle
	record.BeforeHandle = ""
	delete(journal.records, record.Path)
	for index, path := range journal.order {
		if path == record.Path {
			journal.order = append(journal.order[:index], journal.order[index+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if handle != "" {
		_ = m.store.Release(context.Background(), handle)
	}
}

func (m *Manager) release(journal *turnJournal) {
	for _, record := range journal.records {
		if record.BeforeHandle != "" {
			_ = m.store.Release(context.Background(), record.BeforeHandle)
		}
	}
}
