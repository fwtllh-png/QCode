package wire

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
	"github.com/fwtllh-png/QCode/internal/orchestration/chatmerge"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type chatWorkspace struct {
	sessionID string
	threadID  protocol.ThreadID
	worktree  subagent.Worktree
}

// chatWorkspaces binds host sessions to isolated writing threads. Merge,
// journal, and Git behavior belongs to chatmerge.Service.
type chatWorkspaces struct {
	trees      *childWorktrees
	tools      *childToolsets
	threads    *app.ThreadManager
	merger     *chatmerge.Service
	allowApply bool

	mu       sync.Mutex
	sessions map[string]chatWorkspace
}

func newChatWorkspaces(
	trees *childWorktrees,
	tools *childToolsets,
	threads *app.ThreadManager,
	merger *chatmerge.Service,
	allowApply bool,
) *chatWorkspaces {
	if trees == nil || tools == nil || threads == nil || merger == nil {
		return nil
	}
	return &chatWorkspaces{
		trees: trees, tools: tools, threads: threads, merger: merger,
		allowApply: allowApply, sessions: make(map[string]chatWorkspace),
	}
}

func buildChatWorkspaces(
	state *buildState,
	threads *app.ThreadManager,
	gate *agentengine.WorkspaceTurnGate,
) *chatWorkspaces {
	if gate == nil || state.orchestration.chatTrees == nil {
		return nil
	}
	allowApply := state.security.runtime != nil &&
		state.security.runtime.PermissionValue() != policy.PermissionNever
	merger := chatmerge.New(
		state.config.execution.Workspace, state.orchestration.chatTrees.root,
		state.orchestration.parentFiles, state.security.journal,
		gate, state.orchestration.chatTrees.brokers, allowApply,
	)
	return newChatWorkspaces(
		state.orchestration.chatTrees, state.orchestration.childToolsets,
		threads, merger, allowApply,
	)
}

func (c *chatWorkspaces) Provision(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (app.SessionWorkspace, error) {
	if err := c.validateIdentity(sessionID, threadID); err != nil {
		return app.SessionWorkspace{}, err
	}
	worktree, err := c.trees.Provision(chatWorktreeID(sessionID), subagent.StanceWrite)
	if err != nil {
		return app.SessionWorkspace{}, err
	}
	canonical, err := filepath.EvalSymlinks(worktree.Path)
	if err != nil {
		_ = c.trees.Discard(worktree)
		return app.SessionWorkspace{}, err
	}
	worktree.Path = canonical
	if err := c.merger.Snapshot(ctx, worktree.Path); err != nil {
		_ = c.trees.Discard(worktree)
		return app.SessionWorkspace{}, fmt.Errorf("snapshot parent workspace: %w", err)
	}
	value := chatWorkspace{sessionID: sessionID, threadID: threadID, worktree: worktree}
	if err := c.register(value); err != nil {
		_ = c.trees.Discard(worktree)
		return app.SessionWorkspace{}, err
	}
	return app.SessionWorkspace{Mode: app.SessionIsolationWorktree, Root: worktree.Path}, nil
}

func (c *chatWorkspaces) Restore(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (app.SessionWorkspace, error) {
	if err := c.validateIdentity(sessionID, threadID); err != nil {
		return app.SessionWorkspace{}, err
	}
	c.mu.Lock()
	if existing, ok := c.sessions[sessionID]; ok {
		c.mu.Unlock()
		if existing.threadID != threadID {
			return app.SessionWorkspace{}, errors.New("Chat session thread identity mismatch")
		}
		return app.SessionWorkspace{
			Mode: app.SessionIsolationWorktree, Root: existing.worktree.Path,
		}, nil
	}
	c.mu.Unlock()
	path := filepath.Join(c.trees.root, "worktrees", chatWorktreeID(sessionID))
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return app.SessionWorkspace{}, fmt.Errorf("restore Chat worktree: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(c.trees.root)
	if err != nil {
		return app.SessionWorkspace{}, err
	}
	expected := filepath.Join(canonicalRoot, "worktrees", chatWorktreeID(sessionID))
	if canonical != filepath.Clean(expected) {
		return app.SessionWorkspace{}, errors.New("Chat worktree path identity mismatch")
	}
	if err := c.merger.Verify(ctx, canonical); err != nil {
		return app.SessionWorkspace{}, fmt.Errorf("restore Chat worktree HEAD: %w", err)
	}
	value := chatWorkspace{
		sessionID: sessionID, threadID: threadID,
		worktree: subagent.Worktree{
			ID: chatWorktreeID(sessionID), Path: canonical, Isolated: true,
		},
	}
	if err := c.register(value); err != nil {
		return app.SessionWorkspace{}, err
	}
	return app.SessionWorkspace{Mode: app.SessionIsolationWorktree, Root: canonical}, nil
}

func (c *chatWorkspaces) Discard(
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

func (c *chatWorkspaces) PlanMerge(
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

func (c *chatWorkspaces) ApplyMerge(
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

func (c *chatWorkspaces) register(value chatWorkspace) error {
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
	if err := c.threads.RegisterChild(value.threadID, app.ChildSpec{
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

func (c *chatWorkspaces) workspace(
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

func (c *chatWorkspaces) validateIdentity(
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
