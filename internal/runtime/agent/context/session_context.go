package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const ContextSnapshotVersion = 1

type BoundPath struct {
	Path          string `json:"path"`
	ContentDigest string `json:"content_digest"`
}

type WorkspaceBinding struct {
	WorkspaceIdentity string      `json:"workspace_identity"`
	JournalRevision   uint64      `json:"journal_revision"`
	RepositoryHead    string      `json:"repository_head,omitempty"`
	SparseDigest      string      `json:"sparse_digest"`
	BoundPaths        []BoundPath `json:"bound_paths,omitempty"`
}

func (b *WorkspaceBinding) Seal() {
	if b == nil {
		return
	}
	sort.Slice(b.BoundPaths, func(i, j int) bool {
		return b.BoundPaths[i].Path < b.BoundPaths[j].Path
	})
	b.SparseDigest = b.sparseDigest()
}

func (b WorkspaceBinding) Validate() error {
	if strings.TrimSpace(b.WorkspaceIdentity) == "" ||
		b.SparseDigest == "" || b.SparseDigest != b.sparseDigest() {
		return errors.New("workspace binding identity or digest is invalid")
	}
	seen := make(map[string]struct{}, len(b.BoundPaths))
	for _, path := range b.BoundPaths {
		canonical, err := canonicalBoundPath(path.Path)
		if err != nil || path.Path != canonical || path.ContentDigest == "" {
			return errors.New("workspace binding path is invalid")
		}
		if _, duplicate := seen[path.Path]; duplicate {
			return errors.New("workspace binding path is duplicated")
		}
		seen[path.Path] = struct{}{}
	}
	return nil
}

func (b WorkspaceBinding) sparseDigest() string {
	paths := append([]BoundPath(nil), b.BoundPaths...)
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	encoded, _ := json.Marshal(struct {
		WorkspaceIdentity string      `json:"workspace_identity"`
		JournalRevision   uint64      `json:"journal_revision"`
		RepositoryHead    string      `json:"repository_head,omitempty"`
		BoundPaths        []BoundPath `json:"bound_paths,omitempty"`
	}{
		WorkspaceIdentity: b.WorkspaceIdentity,
		JournalRevision:   b.JournalRevision,
		RepositoryHead:    b.RepositoryHead,
		BoundPaths:        paths,
	})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func CaptureWorkspaceBinding(
	root string,
	identity string,
	journalRevision uint64,
	paths []string,
) (WorkspaceBinding, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return WorkspaceBinding{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	if strings.TrimSpace(identity) == "" {
		sum := sha256.Sum256([]byte(filepath.ToSlash(root)))
		identity = "workspace:" + hex.EncodeToString(sum[:])
	}
	seen := make(map[string]struct{}, len(paths))
	binding := WorkspaceBinding{
		WorkspaceIdentity: identity,
		JournalRevision:   journalRevision,
		RepositoryHead:    repositoryHead(root),
	}
	for _, value := range paths {
		path, err := canonicalBoundPath(value)
		if err != nil {
			return WorkspaceBinding{}, fmt.Errorf("bound path %q escapes workspace", value)
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		digest, digestErr := digestWorkspacePath(root, path)
		if digestErr != nil {
			return WorkspaceBinding{}, digestErr
		}
		binding.BoundPaths = append(binding.BoundPaths, BoundPath{
			Path: path, ContentDigest: digest,
		})
	}
	binding.Seal()
	return binding, binding.Validate()
}

func digestWorkspacePath(root, relative string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		path, err = filepath.EvalSymlinks(path)
	case errors.Is(err, os.ErrNotExist):
		path, err = resolveMissingWorkspacePath(path)
	default:
		return "", fmt.Errorf("inspect workspace path %q: %w", relative, err)
	}
	if err != nil {
		return "", fmt.Errorf("resolve workspace path %q: %w", relative, err)
	}
	if !pathWithinRoot(root, path) {
		return "", errors.New("workspace binding path escapes root")
	}
	if info == nil {
		return "missing", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read workspace path %q: %w", relative, err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func repositoryHead(root string) string {
	git, ok := resolveGitDirectory(root)
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(git, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		for _, directory := range gitReferenceDirectories(git) {
			value, readErr := os.ReadFile(
				filepath.Join(directory, filepath.FromSlash(ref)),
			)
			if readErr == nil {
				return strings.TrimSpace(string(value))
			}
			if value := packedReference(directory, ref); value != "" {
				return value
			}
		}
	}
	return head
}

func canonicalBoundPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return "", errors.New("workspace binding path is invalid")
	}
	canonical := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if canonical == "." || canonical == ".." ||
		strings.HasPrefix(canonical, "../") {
		return "", errors.New("workspace binding path is invalid")
	}
	return canonical, nil
}

func resolveMissingWorkspacePath(path string) (string, error) {
	current := filepath.Clean(path)
	var suffix []string
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return "", os.ErrNotExist
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
	}
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func resolveGitDirectory(root string) (string, bool) {
	git := filepath.Join(root, ".git")
	info, err := os.Stat(git)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		resolved, resolveErr := filepath.EvalSymlinks(git)
		return resolved, resolveErr == nil
	}
	raw, err := os.ReadFile(git)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	return resolved, err == nil
}

func gitReferenceDirectories(git string) []string {
	result := []string{git}
	raw, err := os.ReadFile(filepath.Join(git, "commondir"))
	if err != nil {
		return result
	}
	common := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(common) {
		common = filepath.Join(git, common)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(common); resolveErr == nil &&
		resolved != git {
		result = append(result, resolved)
	}
	return result
}

func packedReference(git, ref string) string {
	raw, err := os.ReadFile(filepath.Join(git, "packed-refs"))
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, "^") {
			continue
		}
		value, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return value
		}
	}
	return ""
}

type ContextSnapshot struct {
	Version         int                `json:"version"`
	Epoch           uint64             `json:"epoch"`
	Revision        uint64             `json:"revision"`
	Turn            uint64             `json:"turn,omitempty"`
	History         []provider.Message `json:"history"`
	MessageTurns    []uint64           `json:"message_turns,omitempty"`
	HistoryTurns    map[string]uint64  `json:"history_turns,omitempty"`
	WorkingSet      WorkingSetDelta    `json:"working_set"`
	Evidence        EvidenceDelta      `json:"evidence"`
	Failures        FailureDelta       `json:"failures"`
	Plan            *Plan              `json:"plan,omitempty"`
	World           WorldBaseline      `json:"world,omitempty"`
	Compaction      Compaction         `json:"compaction"`
	TurnCheckpoints []TurnCheckpoint   `json:"turn_checkpoints,omitempty"`
	Workspace       WorkspaceBinding   `json:"workspace"`
	Window          WindowLedger       `json:"window"`
	Digest          string             `json:"digest"`
}

func CloneContextSnapshot(snapshot ContextSnapshot) ContextSnapshot {
	snapshot.History = CloneMessages(snapshot.History)
	snapshot.MessageTurns = append([]uint64(nil), snapshot.MessageTurns...)
	snapshot.HistoryTurns = cloneHistoryTurns(snapshot.HistoryTurns)
	snapshot.WorkingSet.Observations = append(
		[]WorkingSetObservation(nil),
		snapshot.WorkingSet.Observations...,
	)
	snapshot.Evidence.Facts = append(
		[]EvidenceFact(nil),
		snapshot.Evidence.Facts...,
	)
	snapshot.Evidence.Changes = append(
		[]EvidenceChange(nil),
		snapshot.Evidence.Changes...,
	)
	snapshot.Evidence.Reads = append(
		[]EvidenceReadState(nil),
		snapshot.Evidence.Reads...,
	)
	snapshot.Evidence.Handles = append(
		[]EvidenceHandleState(nil),
		snapshot.Evidence.Handles...,
	)
	snapshot.Failures.Failures = append(
		[]Failure(nil),
		snapshot.Failures.Failures...,
	)
	if snapshot.Plan != nil {
		plan := snapshot.Plan.Clone()
		snapshot.Plan = &plan
	}
	snapshot.World = CloneWorldBaseline(snapshot.World)
	snapshot.Compaction = CloneCompaction(snapshot.Compaction)
	snapshot.TurnCheckpoints = CloneTurnCheckpoints(snapshot.TurnCheckpoints)
	snapshot.Workspace.BoundPaths = append(
		[]BoundPath(nil),
		snapshot.Workspace.BoundPaths...,
	)
	snapshot.Window = CloneWindowLedger(snapshot.Window)
	return snapshot
}

func (s *ContextSnapshot) Seal() error {
	if s == nil {
		return errors.New("context snapshot is nil")
	}
	*s = CloneContextSnapshot(*s)
	s.Version = ContextSnapshotVersion
	if len(s.MessageTurns) == 0 && len(s.History) != 0 {
		s.MessageTurns = make([]uint64, len(s.History))
		for index, message := range s.History {
			s.MessageTurns[index] = message.Turn
		}
	}
	if len(s.MessageTurns) == len(s.History) {
		for index, turn := range s.MessageTurns {
			s.History[index].Turn = turn
		}
	}
	s.Workspace.Seal()
	s.Digest = s.digest()
	return s.Validate()
}

func (s ContextSnapshot) Validate() error {
	if s.Version != ContextSnapshotVersion || s.Epoch == 0 ||
		s.Revision == 0 ||
		s.Digest == "" || s.Digest != s.digest() ||
		len(s.MessageTurns) != len(s.History) {
		return errors.New("context snapshot identity or digest is invalid")
	}
	if err := s.Workspace.Validate(); err != nil {
		return err
	}
	if !s.Window.Valid() {
		return errors.New("context snapshot token window is invalid")
	}
	if err := s.Compaction.Validate(); err != nil {
		return err
	}
	if err := ValidateTurnCheckpoints(s.TurnCheckpoints); err != nil {
		return err
	}
	return nil
}

func (s ContextSnapshot) digest() string {
	s.Digest = ""
	encoded, _ := json.Marshal(s)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type AccountingDelta struct {
	TurnID         string         `json:"turn_id"`
	Usage          provider.Usage `json:"usage"`
	CostMicrounits uint64         `json:"cost_microunits"`
	Digest         string         `json:"digest"`
}

func (a *AccountingDelta) Seal() {
	a.Digest = ""
	encoded, _ := json.Marshal(a)
	sum := sha256.Sum256(encoded)
	a.Digest = "sha256:" + hex.EncodeToString(sum[:])
}

func (a AccountingDelta) Validate() error {
	digest := a.Digest
	a.Seal()
	if a.TurnID == "" || digest == "" || digest != a.Digest {
		return errors.New("accounting delta identity or digest is invalid")
	}
	return nil
}

type ReconciliationReceipt struct {
	BindingMatch bool `json:"binding_match"`
	Invalidated  int  `json:"invalidated"`
	Revalidated  int  `json:"revalidated"`
	Stale        int  `json:"stale"`
}

type CurrentContextCommit struct {
	ID             string            `json:"id"`
	ThreadID       protocol.ThreadID `json:"thread_id"`
	TurnID         protocol.TurnID   `json:"turn_id"`
	SessionID      string            `json:"session_id,omitempty"`
	ParentThreadID protocol.ThreadID `json:"parent_thread_id,omitempty"`
	Title          string            `json:"title,omitempty"`
	SourceCursor   protocol.Cursor   `json:"source_cursor,omitempty"`
	ManifestLimits ManifestLimits    `json:"manifest_limits,omitempty"`
	Snapshot       ContextSnapshot   `json:"snapshot"`
}

func (c CurrentContextCommit) Validate() error {
	if strings.TrimSpace(c.ID) == "" || c.ThreadID == "" || c.TurnID == "" {
		return errors.New("current context commit identity is incomplete")
	}
	if err := c.Snapshot.Validate(); err != nil {
		return err
	}
	if state := c.Snapshot.Compaction.State; state != nil &&
		state.ThreadID != c.ThreadID {
		return errors.New("current context compaction belongs to another Thread")
	}
	if c.ParentThreadID == "" {
		if c.SessionID != "" || c.Title != "" || c.SourceCursor != 0 {
			return errors.New("current context commit has incomplete Fork metadata")
		}
		return nil
	}
	if c.ParentThreadID == c.ThreadID || strings.TrimSpace(c.SessionID) == "" ||
		strings.TrimSpace(c.Title) == "" {
		return errors.New("current context Fork metadata is invalid")
	}
	return nil
}

type ContextRebaseEnvelope struct {
	Version             int               `json:"version"`
	CompactionID        string            `json:"compaction_id"`
	ThreadID            protocol.ThreadID `json:"thread_id"`
	TurnID              protocol.TurnID   `json:"turn_id"`
	SourceWindowID      string            `json:"source_window_id"`
	TargetWindowID      string            `json:"target_window_id"`
	BaseRevision        uint64            `json:"base_revision"`
	SourceContextDigest string            `json:"source_context_digest"`
	AuthorityDigest     string            `json:"authority_digest"`
	NarrativeDigest     string            `json:"narrative_digest,omitempty"`
	ManifestLimits      ManifestLimits    `json:"manifest_limits,omitempty"`
	Snapshot            ContextSnapshot   `json:"snapshot"`
	Digest              string            `json:"digest"`
}

func (e *ContextRebaseEnvelope) Seal() error {
	if e == nil {
		return errors.New("context rebase envelope is nil")
	}
	e.Version = 1
	if e.BaseRevision == 0 && e.Snapshot.Revision > 1 {
		e.BaseRevision = e.Snapshot.Revision - 1
	}
	e.Digest = e.digest()
	return e.Validate()
}

func (e ContextRebaseEnvelope) Validate() error {
	if e.Version != 1 || e.CompactionID == "" || e.ThreadID == "" ||
		e.TurnID == "" || e.SourceWindowID == "" ||
		e.TargetWindowID == "" || e.SourceContextDigest == "" ||
		e.AuthorityDigest == "" || e.Digest == "" ||
		e.Digest != e.digest() ||
		e.Snapshot.Revision != e.BaseRevision+1 {
		return errors.New("context rebase envelope identity or digest is invalid")
	}
	if err := e.Snapshot.Validate(); err != nil {
		return err
	}
	if e.Snapshot.Window.ID != e.TargetWindowID {
		return errors.New("context rebase target window does not match snapshot")
	}
	state := e.Snapshot.Compaction.State
	if state == nil || state.ID != e.CompactionID ||
		state.ThreadID != e.ThreadID || state.TurnID != e.TurnID ||
		state.SourceWindowID != e.SourceWindowID ||
		state.TargetWindowID != e.TargetWindowID ||
		state.SourceContextDigest != e.SourceContextDigest {
		return errors.New("context rebase compaction state is inconsistent")
	}
	authorityDigest, err := state.Truth.AuthorityDigest()
	if err != nil || authorityDigest != e.AuthorityDigest {
		return errors.New("context rebase authority is inconsistent")
	}
	if e.NarrativeDigest == "" {
		if state.Narrative != nil {
			return errors.New("context rebase narrative digest is missing")
		}
	} else if state.Narrative == nil ||
		state.Narrative.Digest != e.NarrativeDigest {
		return errors.New("context rebase narrative digest is inconsistent")
	}
	return nil
}

func (e ContextRebaseEnvelope) digest() string {
	e.Digest = ""
	encoded, _ := json.Marshal(e)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ReconcileWorkspace(
	snapshot ContextSnapshot,
	current WorkspaceBinding,
) (ContextSnapshot, ReconciliationReceipt, error) {
	if err := snapshot.Validate(); err != nil {
		return ContextSnapshot{}, ReconciliationReceipt{}, err
	}
	if err := current.Validate(); err != nil {
		return ContextSnapshot{}, ReconciliationReceipt{}, err
	}
	snapshot = CloneContextSnapshot(snapshot)
	receipt := ReconciliationReceipt{
		BindingMatch: snapshot.Workspace.WorkspaceIdentity ==
			current.WorkspaceIdentity,
	}
	checkpointPaths := make(map[string]string, len(snapshot.Workspace.BoundPaths))
	for _, path := range snapshot.Workspace.BoundPaths {
		checkpointPaths[path.Path] = path.ContentDigest
	}
	currentPaths := make(map[string]string, len(current.BoundPaths))
	for _, path := range current.BoundPaths {
		currentPaths[path.Path] = path.ContentDigest
	}
	matches := func(path string) bool {
		return receipt.BindingMatch &&
			checkpointPaths[path] != "" &&
			checkpointPaths[path] == currentPaths[path]
	}
	for index := range snapshot.Evidence.Changes {
		change := &snapshot.Evidence.Changes[index]
		if matches(change.Path) {
			if change.Stale {
				change.Stale = false
				receipt.Revalidated++
			}
			continue
		}
		if change.Verified {
			receipt.Invalidated++
		}
		change.Verified = false
		change.Stale = true
		receipt.Stale++
	}
	for index := range snapshot.Evidence.Facts {
		fact := &snapshot.Evidence.Facts[index]
		if matches(fact.Path) {
			fact.Stale = false
			continue
		}
		fact.Stale = true
		receipt.Invalidated++
		receipt.Stale++
	}
	for index := range snapshot.Evidence.Reads {
		read := &snapshot.Evidence.Reads[index]
		if matches(read.Path) {
			read.Stale = false
			continue
		}
		read.Stale = true
		receipt.Invalidated++
		receipt.Stale++
	}
	if snapshot.Compaction.State != nil {
		state := snapshot.Compaction.State
		truthChanged := false
		removedMessages := 0
		if state.Plan != nil {
			removedMessages = state.Plan.Cut
		}
		for index := range state.Truth.Entities {
			entity := &state.Truth.Entities[index]
			if entity.WorkspacePath == "" {
				continue
			}
			if matches(entity.WorkspacePath) {
				entity.WorkspaceClaimStatus = WorkspaceClaimCurrent
				continue
			}
			if entity.Verified {
				receipt.Invalidated++
			}
			entity.Verified = false
			entity.VerificationSource = ""
			entity.WorkspaceClaimStatus = WorkspaceClaimStale
			receipt.Stale++
			truthChanged = true
		}
		if truthChanged {
			state.Truth.Seal()
			if err := rewriteStructuredHistoryTruth(
				snapshot.History,
				state.Truth,
				removedMessages,
			); err != nil {
				return ContextSnapshot{}, ReconciliationReceipt{}, err
			}
			state.Narrative = nil
			state.NarrativeInput = nil
			state.Plan = nil
			state.Phase = "fallback"
			state.FallbackReason = "workspace_binding_changed"
		}
	}
	snapshot.Workspace = current
	snapshot.World = WorldBaseline{}
	snapshot.Epoch++
	snapshot.Revision++
	snapshot.Window = nextWindow(snapshot.Window, snapshot.Epoch, snapshot.Revision)
	if err := snapshot.Seal(); err != nil {
		return ContextSnapshot{}, ReconciliationReceipt{}, err
	}
	return snapshot, receipt, nil
}

func rewriteStructuredHistoryTruth(
	history []provider.Message,
	truth TruthCapsule,
	removedMessages int,
) error {
	for index := range history {
		if history[index].Role != provider.RoleSystem {
			continue
		}
		if _, structured := Carry(history[index].Text()); !structured {
			continue
		}
		_, found, err := ParseTruthCapsule(history[index].Text())
		if err != nil {
			return fmt.Errorf("parse compacted history truth: %w", err)
		}
		if !found {
			continue
		}
		digest, omitted, _ := CarriedDigest(history[index].Text())
		rendered, err := RenderStructured(
			Summary{
				Window:        removedMessages,
				Digest:        digest,
				OmittedDigest: omitted,
			},
			truth,
			Narrative{},
			0,
		)
		if err != nil {
			return fmt.Errorf("render reconciled history truth: %w", err)
		}
		turn := history[index].Turn
		history[index] = provider.TextMessage(history[index].Role, rendered.Text)
		history[index].Turn = turn
	}
	return nil
}

func nextWindow(
	previous WindowLedger,
	epoch uint64,
	revision uint64,
) WindowLedger {
	id := fmt.Sprintf("context:%d:%d", epoch, revision)
	number := previous.Number + 1
	if number == 1 {
		number = 1
	}
	next, err := NewWindowLedger(id, number)
	if err != nil {
		return WindowLedger{}
	}
	return next
}

func cloneHistoryTurns(input map[string]uint64) map[string]uint64 {
	if input == nil {
		return nil
	}
	result := make(map[string]uint64, len(input))
	for id, turn := range input {
		result[id] = turn
	}
	return result
}
