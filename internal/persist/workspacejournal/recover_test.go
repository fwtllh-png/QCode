package workspacejournal

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A process killed between the before-image and the end of the turn used to
// leave the workspace half changed with nothing to undo it from: the ledger and
// before-images only existed in that process's heap.
func TestTheNextProcessUndoesATurnAKilledProcessLeftHalfApplied(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	if err := killed.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := killed.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "half applied")
	if err := killed.After(path); err != nil {
		t.Fatal(err)
	}
	// No Commit, no Rollback, no Close: the process is gone.

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.RolledBack) != 1 || recovery.RolledBack[0].TurnID != "turn-1" {
		t.Fatalf("recovery = %+v, want turn-1 rolled back", recovery)
	}
	if len(recovery.RolledBack[0].Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", recovery.RolledBack[0].Conflicts)
	}
	if got := readFile(t, path); got != "original" {
		t.Fatalf("file = %q, want the before-image restored", got)
	}
	// Recovery is idempotent: a second process must not find work already done.
	again, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("second recovery = %+v, want nothing left", again)
	}
}

// A committed turn passed its verify gate, so its writes are finished work. A
// crash after the commit must not turn them back.
func TestRecoveryKeepsTheWritesOfATurnThatAlreadyCommitted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	mustRunTurn(t, killed, "turn-1", path, "accepted")
	if err := killed.Commit("turn-1"); err != nil {
		t.Fatal(err)
	}

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.RolledBack) != 0 {
		t.Fatalf("rolled back %+v, want committed work left alone", recovery.RolledBack)
	}
	if len(recovery.Abandoned) != 1 || recovery.Abandoned[0] != "turn-1" {
		t.Fatalf("recovery = %+v, want turn-1 abandoned", recovery)
	}
	if got := readFile(t, path); got != "accepted" {
		t.Fatalf("file = %q, want the committed content kept", got)
	}
}

func TestRecoveryRetainsAndResumesDraftAcrossProcessRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	unchanged := filepath.Join(root, "unchanged.txt")
	writeFile(t, path, "original")
	writeFile(t, unchanged, "same")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	mustRunTurn(t, killed, "source", path, "draft")
	write(t, killed, unchanged, "same")
	if err := killed.Suspend("source"); err != nil {
		t.Fatal(err)
	}

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.RolledBack) != 0 ||
		len(recovery.Drafts) != 1 ||
		recovery.Drafts[0] != "source" ||
		!survivor.HasDraft("source") {
		t.Fatalf("recovery = %+v, draft retained=%v", recovery, survivor.HasDraft("source"))
	}
	if got := readFile(t, path); got != "draft" {
		t.Fatalf("file = %q, want retained draft", got)
	}
	if err := survivor.ResumeDraft("source", "recovery"); err != nil {
		t.Fatal(err)
	}
	write(t, survivor, path, "verified")
	write(t, survivor, unchanged, "updated")
	if err := survivor.Commit("recovery"); err != nil {
		t.Fatal(err)
	}
	if _, err := survivor.Revert(t.Context(), "recovery"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "original" {
		t.Fatalf("reverted recovery = %q, want original baseline", got)
	}
	if got := readFile(t, unchanged); got != "same" {
		t.Fatalf("reverted unchanged record = %q, want original baseline", got)
	}
}

func TestRecoveryDropsLegacyEmptyDraftAcrossProcessRestart(t *testing.T) {
	root := t.TempDir()
	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	if err := killed.Begin("empty"); err != nil {
		t.Fatal(err)
	}
	if err := killed.ledger.append(entry{
		Phase: phaseDraft, TurnID: "empty",
	}); err != nil {
		t.Fatal(err)
	}

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Empty() || survivor.HasDraft("empty") {
		t.Fatalf(
			"recovery = %+v, retained=%v; want empty legacy draft discarded",
			recovery,
			survivor.HasDraft("empty"),
		)
	}
	if err := survivor.Begin("next"); err != nil {
		t.Fatalf("begin after legacy empty draft: %v", err)
	}
}

// Two hosts can share a workspace. Rolling back a turn the other one is still
// running would destroy live work, so an owner that is still alive wins.
func TestRecoverySkipsTurnsWhoseProcessIsStillRunning(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	live := openJournal(t, root)
	if err := live.Begin("turn-live"); err != nil {
		t.Fatal(err)
	}
	if err := live.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "in flight")
	if err := live.After(path); err != nil {
		t.Fatal(err)
	}

	other := openJournal(t, root)
	recovery, err := other.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Skipped) != 1 || recovery.Skipped[0] != "turn-live" {
		t.Fatalf("recovery = %+v, want turn-live skipped", recovery)
	}
	if got := readFile(t, path); got != "in flight" {
		t.Fatalf("file = %q, want the live turn untouched", got)
	}
	// Skipping must not erase the record: the turn still needs undoing if that
	// process dies later.
	pending, err := other.ledger.replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "turn-live" {
		t.Fatalf("ledger = %+v, want turn-live kept", pending)
	}
}

// Before-images have to outlive the process that took them, which means reading
// them back from disk rather than from a map that died with it.
func TestBeforeImagesAreReadableAfterTheProcessThatTookThemIsGone(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original bytes")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	mustRunTurn(t, killed, "turn-1", path, "changed")

	survivor := openJournal(t, root)
	pending, err := survivor.ledger.replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want one turn", pending)
	}
	if len(pending[0].Order) != 1 {
		t.Fatalf("order = %v, want the one path the turn touched", pending[0].Order)
	}
	record := pending[0].Records[pending[0].Order[0]]
	if record == nil || record.BeforeHandle == "" {
		t.Fatalf("record = %+v, want a before-image handle", record)
	}
	data, err := survivor.store.Get(t.Context(), record.BeforeHandle)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original bytes" {
		t.Fatalf("before-image = %q", data)
	}
}

// The recovering process cannot restore a file that changed after the crash: the
// change may be a person's. Reporting the conflict and keeping the record beats
// overwriting it.
func TestRecoveryReportsAConflictAndKeepsTheRecordWhenTheFileMovedOn(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	mustRunTurn(t, killed, "turn-1", path, "half applied")
	writeFile(t, path, "someone edited this by hand")

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.RolledBack) != 1 || len(recovery.RolledBack[0].Conflicts) != 1 {
		t.Fatalf("recovery = %+v, want one conflict", recovery)
	}
	if got := readFile(t, path); got != "someone edited this by hand" {
		t.Fatalf("file = %q, want the hand edit preserved", got)
	}
	pending, err := survivor.ledger.replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("ledger = %+v, want the unresolved turn kept", pending)
	}
}

// A crash can tear the line being appended. That line describes a write that had
// not happened yet, so ignoring it is safe — and the turns before it must still
// replay.
func TestReplayIgnoresATornFinalLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	killed := openJournal(t, root)
	killed.pid = deadPID(t)
	mustRunTurn(t, killed, "turn-1", path, "half applied")
	ledgerPath := filepath.Join(
		filepath.Dir(root),
		"."+filepath.Base(root)+"-journal",
		ledgerName,
	)
	if err := killed.ledger.close(); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, ledgerPath, `{"phase":"before","turn_id":"turn-2","rec`)

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.RolledBack) != 1 || recovery.RolledBack[0].TurnID != "turn-1" {
		t.Fatalf("recovery = %+v, want turn-1 rolled back", recovery)
	}
	if got := readFile(t, path); got != "original" {
		t.Fatalf("file = %q, want the before-image restored", got)
	}
}

func TestReplayRejectsMalformedCompleteLine(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, filepath.Join(directory, ledgerName), "{malformed}\n")
	reopened, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, err := reopened.Recover(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "decode workspace journal") {
		t.Fatalf("Recover() error = %v", err)
	}
}

// A clean shutdown should leave nothing for the next process to reason about.
func TestClosingSettlesCommittedTurnsSoTheNextProcessStartsClean(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	manager := openJournal(t, root)
	mustRunTurn(t, manager, "turn-1", path, "accepted")
	if err := manager.Commit("turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	survivor := openJournal(t, root)
	recovery, err := survivor.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Empty() {
		t.Fatalf("recovery after clean shutdown = %+v, want nothing", recovery)
	}
	if got := readFile(t, path); got != "accepted" {
		t.Fatalf("file = %q", got)
	}
}

func TestClosingHandsDraftToInProcessReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	writeFile(t, path, "original")

	manager := openJournal(t, root)
	mustRunTurn(t, manager, "source", path, "draft")
	if err := manager.Suspend("source"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	replacement := openJournal(t, root)
	recovery, err := replacement.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Drafts) != 1 ||
		recovery.Drafts[0] != "source" ||
		!replacement.HasDraft("source") {
		t.Fatalf("replacement recovery = %+v", recovery)
	}
}

func openJournal(t *testing.T, root string) *Manager {
	t.Helper()
	directory := filepath.Join(
		filepath.Dir(root),
		"."+filepath.Base(root)+"-journal",
	)
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestOpenResolvesRelativeExternalJournalDirectory(t *testing.T) {
	root := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	manager, err := Open(
		".",
		filepath.Join("..", "."+filepath.Base(root)+"-journal"),
		testWorkspaceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := os.Stat(filepath.Join(
		"..",
		"."+filepath.Base(root)+"-journal",
		ledgerName,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("ledger mode = %v", info.Mode())
	}
}

func TestOpenRejectsJournalInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, ".qcode", "journal")
	_, err := Open(
		root,
		directory,
		testWorkspaceID,
	)
	if err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("rejected journal directory was created: %v", err)
	}
}

func TestOpenRejectsWorkspaceBindingReuse(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	first, err := Open(root, directory, strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, directory, strings.Repeat("2", 64)); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Open() binding error = %v", err)
	}
}

func TestDurableLedgerStoresWorkspaceRelativePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if err := manager.Begin("relative-path"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, ledgerName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), root) ||
		!strings.Contains(string(data), `"path":"value.txt"`) {
		t.Fatalf("ledger contains unsafe path representation: %s", data)
	}
}

func TestRecoverRejectsAbsoluteLedgerPath(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"begin","turn_id":"hostile","owner":"dead","pid":0}`+"\n")
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"after","turn_id":"hostile","record":{"path":`+
			strconv.Quote(outside)+`,"before":{"path":`+
			strconv.Quote(outside)+`},"after":{"path":`+
			strconv.Quote(outside)+`}}}`+"\n")
	reopened, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, err := reopened.Recover(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("Recover() error = %v", err)
	}
}

func TestRecoverRejectsSymlinkEscapeFromLedger(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"begin","turn_id":"hostile","owner":"dead","pid":0}`+"\n")
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"after","turn_id":"hostile","record":{"path":"escape/file",`+
			`"before":{"path":"escape/file"},"after":{"path":"escape/file"}}}`+"\n")
	reopened, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, err := reopened.Recover(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Recover() error = %v", err)
	}
}

func TestOpenRejectsReplacedWorkspaceRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, directory, testWorkspaceID); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Open() root replacement error = %v", err)
	}
}

// A volume device number is not stable across reboots (macOS reassigns
// st_dev per mount set), while the workspace directory keeps its inode. The
// journal must open against the drifted device and refresh the stored
// binding rather than refuse to start.
func TestOpenToleratesDeviceDriftAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	bindingPath := filepath.Join(directory, bindingName)
	raw, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	var drifted workspaceBinding
	if err := json.Unmarshal(raw, &drifted); err != nil {
		t.Fatal(err)
	}
	drifted.Device += 4
	drifted.FileID = "drifted-file-id"
	encoded, err := json.Marshal(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bindingPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatalf("Open() after device drift = %v", err)
	}
	if err := reopened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	refreshed, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	var current workspaceBinding
	if err := json.Unmarshal(refreshed, &current); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	currentIdentity := identity(info)
	if current.Device != currentIdentity.Device ||
		current.Inode != currentIdentity.Inode ||
		current.FileID != currentIdentity.FileID {
		t.Fatalf("refreshed binding = %+v, want identity %+v", current, currentIdentity)
	}
}

func TestOpenRejectsSymlinkOrHardlinkedControlFiles(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	originalBinding := filepath.Join(directory, bindingName)
	movedBinding := originalBinding + ".moved"
	if err := os.Rename(originalBinding, movedBinding); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(movedBinding, originalBinding); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Open(root, directory, testWorkspaceID); err == nil ||
		!strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("Open() symlink binding error = %v", err)
	}
	if err := os.Remove(originalBinding); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedBinding, originalBinding); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(directory, ledgerName)
	if err := os.Link(ledgerPath, ledgerPath+".alias"); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := Open(root, directory, testWorkspaceID); err == nil ||
		!strings.Contains(err.Error(), "multiply linked") {
		t.Fatalf("Open() hardlinked ledger error = %v", err)
	}
}

func TestRecoverRejectsMultiplyLinkedTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(root, "alias.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"begin","turn_id":"hostile","owner":"dead","pid":0}`+"\n")
	appendRaw(t, filepath.Join(directory, ledgerName),
		`{"phase":"after","turn_id":"hostile","record":{"path":"target.txt",`+
			`"before":{"path":"target.txt"},"after":{"path":"target.txt"}}}`+"\n")
	reopened, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, err := reopened.Recover(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "multiply linked") {
		t.Fatalf("Recover() error = %v", err)
	}
}

func mustRunTurn(t *testing.T, manager *Manager, turnID, path, content string) {
	t.Helper()
	if err := manager.Begin(turnID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
	if err := manager.After(path); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func appendRaw(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns the pid of a process that has exited and been reaped, which is
// what a killed host looks like to the next one.
func deadPID(t *testing.T) int {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return command.Process.Pid
}
