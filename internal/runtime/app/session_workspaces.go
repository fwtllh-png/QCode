package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type chatWorkspace struct {
	sessionID string
	threadID  protocol.ThreadID
	worktree  subagent.Worktree
}

// IsolatedSessionWorkspaces owns session-to-thread bindings and isolated workspace lifecycles.
type IsolatedSessionWorkspaces struct {
	trees      subagent.WorktreeProvider
	tools      SessionWorkspaceTools
	threads    SessionWorkspaceThreads
	merger     SessionWorkspaceMerger
	root       string
	allowApply bool

	mu       sync.Mutex
	sessions map[string]chatWorkspace
}

// SessionWorkspaceThreads registers and releases a session's isolated engine.
type SessionWorkspaceThreads interface {
	RegisterChild(protocol.ThreadID, ChildSpec) error
	Release(protocol.ThreadID)
}

// SessionWorkspaceTools releases the tools rooted at a discarded workspace.
type SessionWorkspaceTools interface{ Release(string) }

// SessionWorkspaceMerger owns workspace snapshots, merge plans and guarded apply.
type SessionWorkspaceMerger interface {
	Snapshot(context.Context, string) error
	Verify(context.Context, string) error
	Plan(context.Context, string) (tool.EditPlan, error)
	Apply(context.Context, string, string, string) (tool.EditPlan, error)
}

type IsolatedSessionWorkspaceOptions struct {
	Root       string
	Trees      subagent.WorktreeProvider
	Tools      SessionWorkspaceTools
	Threads    SessionWorkspaceThreads
	Merger     SessionWorkspaceMerger
	AllowApply bool
}

func NewIsolatedSessionWorkspaces(options IsolatedSessionWorkspaceOptions) *IsolatedSessionWorkspaces {
	if options.Trees == nil || options.Tools == nil || options.Threads == nil || options.Merger == nil {
		return nil
	}
	return &IsolatedSessionWorkspaces{
		root: options.Root, trees: options.Trees, tools: options.Tools,
		threads: options.Threads, merger: options.Merger, allowApply: options.AllowApply,
		sessions: make(map[string]chatWorkspace),
	}
}

func (c *IsolatedSessionWorkspaces) Provision(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (SessionWorkspace, error) {
	if err := c.validateIdentity(sessionID, threadID); err != nil {
		return SessionWorkspace{}, err
	}
	worktree, err := c.trees.Provision(chatWorktreeID(sessionID), subagent.StanceWrite)
	if err != nil {
		return SessionWorkspace{}, err
	}
	canonical, err := filepath.EvalSymlinks(worktree.Path)
	if err != nil {
		_ = c.trees.Discard(worktree)
		return SessionWorkspace{}, err
	}
	worktree.Path = canonical
	if err := c.merger.Snapshot(ctx, worktree.Path); err != nil {
		_ = c.trees.Discard(worktree)
		return SessionWorkspace{}, fmt.Errorf("snapshot parent workspace: %w", err)
	}
	value := chatWorkspace{sessionID: sessionID, threadID: threadID, worktree: worktree}
	if err := c.register(value); err != nil {
		_ = c.trees.Discard(worktree)
		return SessionWorkspace{}, err
	}
	return SessionWorkspace{Mode: SessionIsolationWorktree, Root: worktree.Path}, nil
}

func (c *IsolatedSessionWorkspaces) Restore(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (SessionWorkspace, error) {
	if err := c.validateIdentity(sessionID, threadID); err != nil {
		return SessionWorkspace{}, err
	}
	c.mu.Lock()
	if existing, ok := c.sessions[sessionID]; ok {
		c.mu.Unlock()
		if existing.threadID != threadID {
			return SessionWorkspace{}, errors.New("Chat session thread identity mismatch")
		}
		return SessionWorkspace{
			Mode: SessionIsolationWorktree, Root: existing.worktree.Path,
		}, nil
	}
	c.mu.Unlock()
	path := filepath.Join(c.root, "worktrees", chatWorktreeID(sessionID))
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return SessionWorkspace{}, fmt.Errorf("restore Chat worktree: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(c.root)
	if err != nil {
		return SessionWorkspace{}, err
	}
	expected := filepath.Join(canonicalRoot, "worktrees", chatWorktreeID(sessionID))
	if canonical != filepath.Clean(expected) {
		return SessionWorkspace{}, errors.New("Chat worktree path identity mismatch")
	}
	if err := c.merger.Verify(ctx, canonical); err != nil {
		return SessionWorkspace{}, fmt.Errorf("restore Chat worktree HEAD: %w", err)
	}
	value := chatWorkspace{
		sessionID: sessionID, threadID: threadID,
		worktree: subagent.Worktree{
			ID: chatWorktreeID(sessionID), Path: canonical, Isolated: true,
		},
	}
	if err := c.register(value); err != nil {
		return SessionWorkspace{}, err
	}
	return SessionWorkspace{Mode: SessionIsolationWorktree, Root: canonical}, nil
}

func (c *IsolatedSessionWorkspaces) Discard(
	_ context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) error {
	c.mu.Lock()
	value, ok := c.sessions[sessionID]
	if ok {
		delete(c.sessions, sessionID)
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	if value.threadID != threadID {
		return errors.New("Chat session thread identity mismatch")
	}
	c.threads.Release(threadID)
	c.tools.Release(value.worktree.Path)
	return c.trees.Discard(value.worktree)
}

func (c *IsolatedSessionWorkspaces) PlanMerge(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (tool.EditPlan, error) {
	workspace, err := c.workspace(sessionID, threadID)
	if err != nil {
		return tool.EditPlan{}, err
	}
	return c.merger.Plan(ctx, workspace.worktree.Path)
}

func (c *IsolatedSessionWorkspaces) ApplyMerge(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
	planID string,
) (tool.EditPlan, error) {
	if !c.allowApply {
		return tool.EditPlan{}, errors.New(
			"Chat merge apply is unavailable in a read-only workspace",
		)
	}
	workspace, err := c.workspace(sessionID, threadID)
	if err != nil {
		return tool.EditPlan{}, err
	}
	return c.merger.Apply(ctx, sessionID, workspace.worktree.Path, planID)
}

func (c *IsolatedSessionWorkspaces) register(value chatWorkspace) error {
	c.mu.Lock()
	if existing, ok := c.sessions[value.sessionID]; ok {
		c.mu.Unlock()
		if existing.threadID == value.threadID &&
			existing.worktree.Path == value.worktree.Path {
			return nil
		}
		return errors.New("Chat session is already bound to another worktree")
	}
	c.mu.Unlock()
	if err := c.threads.RegisterChild(value.threadID, ChildSpec{
		AgentID: value.sessionID, SessionID: value.sessionID, Role: "chat", Stance: string(subagent.StanceWrite),
		Workspace: value.worktree.Path, HostSeeded: true,
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.sessions[value.sessionID] = value
	c.mu.Unlock()
	return nil
}

func (c *IsolatedSessionWorkspaces) workspace(
	sessionID string,
	threadID protocol.ThreadID,
) (chatWorkspace, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.sessions[sessionID]
	if !ok {
		return chatWorkspace{}, errors.New("Chat session has no isolated worktree")
	}
	if value.threadID != threadID {
		return chatWorkspace{}, errors.New("Chat session thread identity mismatch")
	}
	return value, nil
}

func (c *IsolatedSessionWorkspaces) validateIdentity(
	sessionID string,
	threadID protocol.ThreadID,
) error {
	if c == nil {
		return errors.New("isolated Chat workspaces are unavailable")
	}
	if strings.TrimSpace(sessionID) == "" || threadID == "" {
		return errors.New("Chat session and thread ids are required")
	}
	return nil
}

func chatWorktreeID(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return "chat-" + hex.EncodeToString(sum[:16])
}
