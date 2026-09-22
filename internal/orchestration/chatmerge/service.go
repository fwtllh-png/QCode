// Package chatmerge owns isolated Chat workspace snapshots and guarded merges.
package chatmerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const (
	maxChatMergeFiles     = 512
	chatMergeBatchFiles   = 64
	maxChatMergeDiffBytes = 3 << 20
)

var ErrWorkspaceClean = errors.New("Chat worktree has no changes to merge")

// Service owns merge planning, journaling, baseline snapshots, and Git calls.
type Service struct {
	repository string
	root       string
	parent     *filetool.Tools
	journal    *workspacejournal.Manager
	gate       *agentengine.WorkspaceTurnGate
	brokers    WorkspaceBroker
	allowApply bool
}

type WorkspaceBroker interface {
	ReadVCS(context.Context, string, ...string) (string, error)
	AddWorktree(context.Context, string, string, string) error
	RemoveWorktree(context.Context, string, string) error
	PruneWorktrees(context.Context, string) error
	ApplyPatch(context.Context, string, string) error
	AddIndex(context.Context, string, []string) error
	CommitBaseline(context.Context, string) error
	CommitFiles(
		context.Context,
		string,
		filebroker.Plan,
		*workspacejournal.Manager,
	) (filebroker.Result, error)
	CommitFilesAt(
		context.Context,
		string,
		string,
		filebroker.Plan,
	) (filebroker.Result, error)
}

// New returns nil when the merge boundary is unavailable.
func New(
	repository, root string,
	parent *filetool.Tools,
	journal *workspacejournal.Manager,
	gate *agentengine.WorkspaceTurnGate,
	brokers WorkspaceBroker,
	allowApply bool,
) *Service {
	if parent == nil || journal == nil || gate == nil ||
		brokers == nil {
		return nil
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return nil
	}
	return &Service{
		repository: canonicalRepository, root: root,
		parent: parent, journal: journal, gate: gate,
		brokers: brokers, allowApply: allowApply,
	}
}

// Apply recomputes a preview-bound plan and commits it through the journal.
func (c *Service) Apply(
	ctx context.Context,
	sessionID string,
	worktree string,
	planID string,
) (_ tool.EditPlan, resultErr error) {
	if !c.allowApply {
		return tool.EditPlan{}, errors.New(
			"Chat merge apply is unavailable in a read-only workspace",
		)
	}
	if len(planID) != 64 {
		return tool.EditPlan{}, errors.New("Chat merge plan id is invalid")
	}
	release, err := c.gate.Acquire(ctx)
	if err != nil {
		return tool.EditPlan{}, err
	}
	defer release()

	plan, err := c.plan(ctx, worktree, nil)
	if err != nil {
		return tool.EditPlan{}, err
	}
	if plan.edit.ID != planID {
		return tool.EditPlan{}, errors.New("Chat merge plan is stale")
	}
	transactionID := "chat-merge-" + sessionID
	if err := c.journal.Begin(transactionID); err != nil {
		return tool.EditPlan{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		receipt, rollbackErr := c.journal.Rollback(context.WithoutCancel(ctx), transactionID)
		if len(receipt.Conflicts) != 0 {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf("Chat merge rollback left %d conflict(s)", len(receipt.Conflicts)),
			)
		}
		resultErr = errors.Join(resultErr, rollbackErr)
	}()
	if _, err := c.brokers.CommitFiles(
		ctx, "chat_merge", plan.filePlan, c.journal,
	); err != nil {
		return tool.EditPlan{}, fmt.Errorf("apply Chat merge: %w", err)
	}
	if err := c.journal.Commit(transactionID); err != nil {
		committed = true
		return tool.EditPlan{}, err
	}
	committed = true
	if err := c.syncChatWorktreeFromParent(
		ctx, plan.worktree, plan.paths,
	); err != nil {
		return tool.EditPlan{}, fmt.Errorf(
			"Chat changes reached the main workspace but worktree refresh failed: %w", err,
		)
	}
	if err := c.commitBaseline(ctx, plan.worktree, plan.paths); err != nil {
		return tool.EditPlan{}, fmt.Errorf(
			"Chat changes reached the main workspace but baseline refresh failed: %w", err,
		)
	}
	return plan.edit, nil
}

// ApplyPaths applies a prefix-filtered merge through the current journal
// turn. Callers already holding the workspace gate (a running Turn or an
// isolated command) must not acquire it again.
func (c *Service) ApplyPaths(
	ctx context.Context,
	transactionID string,
	worktree string,
	planID string,
	prefixes []string,
) (tool.EditPlan, error) {
	if !c.allowApply {
		return tool.EditPlan{}, errors.New(
			"isolated workspace merge is unavailable in a read-only workspace",
		)
	}
	if len(planID) != 64 {
		return tool.EditPlan{}, errors.New("isolated workspace merge plan id is invalid")
	}
	if strings.TrimSpace(transactionID) == "" {
		return tool.EditPlan{}, errors.New("isolated workspace merge transaction is required")
	}
	plan, err := c.plan(ctx, worktree, prefixes)
	if err != nil {
		return tool.EditPlan{}, err
	}
	if plan.edit.ID != planID {
		return tool.EditPlan{}, errors.New("isolated workspace merge plan is stale")
	}
	if _, err := c.brokers.CommitFiles(
		ctx, "exec_settle", plan.filePlan, c.journal,
	); err != nil {
		return tool.EditPlan{}, fmt.Errorf("apply isolated workspace merge: %w", err)
	}
	return plan.edit, nil
}

type preparedChatMerge struct {
	worktree string
	edit     tool.EditPlan
	paths    []string
	filePlan filebroker.Plan
}

// Plan returns a compact, digest-bound merge preview.
func (c *Service) Plan(ctx context.Context, worktree string) (tool.EditPlan, error) {
	return c.PlanPaths(ctx, worktree, nil)
}

// PlanPaths plans a merge for changed paths under prefixes. Empty prefixes
// include every changed path.
func (c *Service) PlanPaths(
	ctx context.Context, worktree string, prefixes []string,
) (tool.EditPlan, error) {
	plan, err := c.plan(ctx, worktree, prefixes)
	if err != nil {
		return tool.EditPlan{}, err
	}
	return plan.edit, nil
}

func (c *Service) plan(
	ctx context.Context, worktree string, prefixes []string,
) (preparedChatMerge, error) {
	paths, err := c.changedPaths(ctx, worktree, prefixes)
	if err != nil {
		return preparedChatMerge{}, err
	}
	if len(paths) == 0 {
		return preparedChatMerge{}, ErrWorkspaceClean
	}
	if len(paths) > maxChatMergeFiles {
		return preparedChatMerge{}, fmt.Errorf(
			"Chat merge has %d files; at most %d are allowed", len(paths), maxChatMergeFiles,
		)
	}
	changes := make([]filetool.Change, 0, len(paths))
	expected := make(map[string]workspacejournal.Fingerprint, len(paths))
	for _, path := range paths {
		parentPath := filepath.Join(c.repository, filepath.FromSlash(path))
		fingerprint, _, _, err := workspacejournal.Snapshot(parentPath)
		if err != nil {
			return preparedChatMerge{}, err
		}
		fingerprint.Path = parentPath
		expected[fingerprint.Path] = fingerprint
		change, required, err := c.mergeChatPath(
			ctx, worktree, path,
		)
		if err != nil {
			return preparedChatMerge{}, err
		}
		if required {
			changes = append(changes, change)
		}
	}
	if len(changes) == 0 {
		return preparedChatMerge{}, ErrWorkspaceClean
	}
	planContext := workspacejournal.WithExpectedWrites(ctx, expected)
	batches := chunkChatMergeChanges(changes)
	edit := tool.EditPlan{}
	var diff strings.Builder
	var fileEntries []filebroker.Entry
	for index, batch := range batches {
		batchPlan, err := c.parent.PlanApply(planContext, batch)
		if err != nil {
			return preparedChatMerge{}, fmt.Errorf(
				"plan Chat merge batch %d/%d: %w",
				index+1, len(batches), err,
			)
		}
		prepared, err := c.parent.PrepareApply(planContext, batch)
		if err != nil {
			return preparedChatMerge{}, fmt.Errorf(
				"prepare Chat merge batch %d/%d: %w",
				index+1, len(batches), err,
			)
		}
		fileEntries = append(fileEntries, prepared.Plan.Entries...)
		if diff.Len() != 0 && !strings.HasSuffix(diff.String(), "\n") {
			diff.WriteByte('\n')
		}
		diff.WriteString(batchPlan.Diff)
		if diff.Len() > maxChatMergeDiffBytes {
			return preparedChatMerge{}, fmt.Errorf(
				"Chat merge diff exceeds %d bytes", maxChatMergeDiffBytes,
			)
		}
		for _, file := range batchPlan.Files {
			edit.Files = append(edit.Files, compactChatMergePlanFile(file))
		}
	}
	filePlan, err := filebroker.NewPlan(fileEntries)
	if err != nil {
		return preparedChatMerge{}, err
	}
	edit.ID = filePlan.Digest
	edit.Diff = diff.String()
	return preparedChatMerge{
		worktree: worktree, edit: edit,
		paths: paths, filePlan: filePlan,
	}, nil
}

func chunkChatMergeChanges(changes []filetool.Change) [][]filetool.Change {
	batches := make([][]filetool.Change, 0, (len(changes)+chatMergeBatchFiles-1)/chatMergeBatchFiles)
	for start := 0; start < len(changes); start += chatMergeBatchFiles {
		end := min(start+chatMergeBatchFiles, len(changes))
		batches = append(batches, changes[start:end])
	}
	return batches
}

func compactChatMergePlanFile(file tool.EditPlanFile) tool.EditPlanFile {
	file.Before = ""
	file.After = ""
	return file
}

// Snapshot copies parent changes into a new worktree and commits its baseline.
func (c *Service) Snapshot(ctx context.Context, worktree string) error {
	diff, err := c.git(
		ctx, c.repository, "diff", "--binary", "--no-ext-diff", "HEAD",
		"--", ".", ":(exclude).qcode",
	)
	if err != nil {
		return err
	}
	if diff != "" {
		file, err := os.CreateTemp(c.root, "chat-parent-*.patch")
		if err != nil {
			return err
		}
		name := file.Name()
		if _, err := file.WriteString(diff); err != nil {
			_ = file.Close()
			_ = os.Remove(name)
			return err
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(name)
			return err
		}
		defer os.Remove(name)
		if err := c.brokers.ApplyPatch(ctx, worktree, name); err != nil {
			return err
		}
	}
	untracked, err := c.git(
		ctx, c.repository, "ls-files", "--others", "--exclude-standard", "-z",
		"--", ".", ":(exclude).qcode",
	)
	if err != nil {
		return err
	}
	if err := c.syncWorkspaceFiles(
		ctx, c.repository, worktree, splitNUL(untracked),
	); err != nil {
		return err
	}
	return c.commitBaseline(ctx, worktree, nil)
}

// Verify rejects a restored worktree without its committed baseline.
func (c *Service) Verify(ctx context.Context, worktree string) error {
	_, err := c.git(ctx, worktree, "rev-parse", "--verify", "HEAD")
	return err
}

func (c *Service) changedPaths(
	ctx context.Context,
	worktree string,
	prefixes []string,
) ([]string, error) {
	tracked, err := c.git(
		ctx, worktree, "diff", "--name-only", "-z", "HEAD",
		"--", ".", ":(exclude).qcode",
	)
	if err != nil {
		return nil, err
	}
	untracked, err := c.git(
		ctx, worktree, "ls-files", "--others", "--exclude-standard", "-z",
		"--", ".", ":(exclude).qcode",
	)
	if err != nil {
		return nil, err
	}
	unique := make(map[string]struct{})
	for _, path := range append(splitNUL(tracked), splitNUL(untracked)...) {
		path = filepath.ToSlash(filepath.Clean(path))
		if path == "." ||
			path == ".qcode" || strings.HasPrefix(path, ".qcode/") {
			continue
		}
		if !matchPathPrefix(path, prefixes) {
			continue
		}
		unique[path] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for path := range unique {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

type chatMergeFile struct {
	exists bool
	data   []byte
}

func (c *Service) mergeChatPath(
	ctx context.Context,
	worktree string,
	path string,
) (filetool.Change, bool, error) {
	base, err := c.chatBaselineFile(ctx, worktree, path)
	if err != nil {
		return filetool.Change{}, false, err
	}
	parent, err := readChatMergeFile(
		filepath.Join(c.repository, filepath.FromSlash(path)),
	)
	if err != nil {
		return filetool.Change{}, false, err
	}
	child, err := readChatMergeFile(
		filepath.Join(worktree, filepath.FromSlash(path)),
	)
	if err != nil {
		return filetool.Change{}, false, err
	}
	if equalChatMergeFile(parent, child) {
		return filetool.Change{}, false, nil
	}
	if equalChatMergeFile(parent, base) {
		return chatMergeChange(path, child), true, nil
	}
	if equalChatMergeFile(child, base) {
		return filetool.Change{}, false, nil
	}
	if !base.exists || !parent.exists || !child.exists {
		return filetool.Change{}, false, fmt.Errorf(
			"Chat merge conflict on %s: main workspace drifted with overlapping changes",
			path,
		)
	}
	merged, err := c.mergeChatText(ctx, path, parent.data, base.data, child.data)
	if err != nil {
		return filetool.Change{}, false, err
	}
	desired := chatMergeFile{exists: true, data: merged}
	if equalChatMergeFile(parent, desired) {
		return filetool.Change{}, false, nil
	}
	return chatMergeChange(path, desired), true, nil
}

func (c *Service) chatBaselineFile(
	ctx context.Context,
	worktree string,
	path string,
) (chatMergeFile, error) {
	result, err := process.Run(ctx, process.Options{
		Path: process.GitExecutable(),
		Args: process.ManagedGitArguments([]string{"show", "HEAD:" + path}),
		Dir:  worktree,
	})
	if err != nil {
		return chatMergeFile{}, err
	}
	if result.ExitCode != 0 {
		return chatMergeFile{}, nil
	}
	data := []byte(result.Stdout)
	if !utf8.Valid(data) {
		return chatMergeFile{}, fmt.Errorf(
			"Chat merge baseline %q is not UTF-8 text", path,
		)
	}
	return chatMergeFile{exists: true, data: data}, nil
}

func readChatMergeFile(path string) (chatMergeFile, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !utf8.Valid(data) {
			return chatMergeFile{}, fmt.Errorf(
				"Chat merge path %q is not UTF-8 text", path,
			)
		}
		return chatMergeFile{exists: true, data: data}, nil
	case errors.Is(err, os.ErrNotExist):
		return chatMergeFile{}, nil
	default:
		return chatMergeFile{}, err
	}
}

func equalChatMergeFile(left, right chatMergeFile) bool {
	return left.exists == right.exists &&
		(!left.exists || bytes.Equal(left.data, right.data))
}

func chatMergeChange(path string, file chatMergeFile) filetool.Change {
	if !file.exists {
		return filetool.Change{Op: "delete", Path: path}
	}
	return filetool.Change{Op: "write", Path: path, Content: string(file.data)}
}

func (c *Service) mergeChatText(
	ctx context.Context,
	path string,
	parent []byte,
	base []byte,
	child []byte,
) ([]byte, error) {
	directory, err := os.MkdirTemp(c.root, "chat-merge-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	files := []struct {
		name string
		data []byte
	}{
		{name: "parent", data: parent},
		{name: "base", data: base},
		{name: "child", data: child},
	}
	for _, file := range files {
		if err := os.WriteFile(
			filepath.Join(directory, file.name), file.data, 0o600,
		); err != nil {
			return nil, err
		}
	}
	result, err := process.Run(ctx, process.Options{
		Path: process.GitExecutable(),
		Args: process.ManagedGitArguments([]string{
			"merge-file", "-p",
			filepath.Join(directory, "parent"),
			filepath.Join(directory, "base"),
			filepath.Join(directory, "child"),
		}),
		Dir: c.repository,
	})
	if err != nil {
		return nil, err
	}
	switch result.ExitCode {
	case 0:
		return []byte(result.Stdout), nil
	case 1:
		return nil, fmt.Errorf(
			"Chat merge conflict on %s: main workspace drifted with overlapping changes",
			path,
		)
	default:
		return nil, fmt.Errorf(
			"Chat merge failed on %s: %s",
			path, strings.TrimSpace(result.Stderr),
		)
	}
}

func (c *Service) syncChatWorktreeFromParent(
	ctx context.Context,
	worktree string,
	paths []string,
) error {
	return c.syncWorkspaceFiles(ctx, c.repository, worktree, paths)
}

func (c *Service) commitBaseline(
	ctx context.Context,
	worktree string,
	paths []string,
) error {
	var pathsToAdd []string
	if len(paths) == 0 {
		pathsToAdd = []string{".", ":(exclude).qcode"}
	} else {
		pathsToAdd = append(pathsToAdd, paths...)
	}
	if err := c.brokers.AddIndex(ctx, worktree, pathsToAdd); err != nil {
		return err
	}
	return c.brokers.CommitBaseline(ctx, worktree)
}

func (c *Service) git(
	ctx context.Context,
	directory string,
	arguments ...string,
) (string, error) {
	return c.brokers.ReadVCS(ctx, directory, arguments...)
}

func matchPathPrefix(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		prefix = filepath.ToSlash(filepath.Clean(strings.TrimSpace(prefix)))
		if prefix == "" || prefix == "." {
			return true
		}
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func splitNUL(value string) []string {
	parts := strings.Split(value, "\x00")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (c *Service) syncWorkspaceFiles(
	ctx context.Context,
	sourceRoot, targetRoot string,
	paths []string,
) error {
	if len(paths) == 0 {
		return nil
	}
	source, err := sandbox.NewWorkspace(sourceRoot)
	if err != nil {
		return err
	}
	target, err := sandbox.NewWorkspace(targetRoot)
	if err != nil {
		return err
	}
	entries := make([]filebroker.Entry, 0, len(paths))
	for _, path := range paths {
		sourceFile, err := source.SnapshotFile(path)
		if err != nil {
			return fmt.Errorf("snapshot Chat source %q: %w", path, err)
		}
		targetFile, err := target.SnapshotFile(path)
		if err != nil {
			return fmt.Errorf("snapshot Chat target %q: %w", path, err)
		}
		if sourceFile.Exists == targetFile.Exists &&
			(!sourceFile.Exists ||
				(sourceFile.Digest == targetFile.Digest &&
					sourceFile.Mode.Perm() == targetFile.Mode.Perm())) {
			continue
		}
		entry := filebroker.Entry{
			Path: filepath.ToSlash(filepath.Clean(path)),
			Before: filebroker.State{
				Exists: targetFile.Exists, Digest: targetFile.Digest,
				Identity: targetFile.Identity, Mode: uint32(targetFile.Mode.Perm()),
			},
			BeforeData: targetFile.Data,
		}
		if sourceFile.Exists {
			entry.After = filebroker.State{
				Exists: true, Digest: sourceFile.Digest,
				Mode: uint32(sourceFile.Mode.Perm()),
			}
			entry.Data = sourceFile.Data
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil
	}
	plan, err := filebroker.NewPlan(entries)
	if err != nil {
		return err
	}
	_, err = c.brokers.CommitFilesAt(
		ctx, targetRoot, "chat_worktree_sync", plan,
	)
	return err
}
