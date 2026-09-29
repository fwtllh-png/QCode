package subagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) reconcileOrphanWorktrees() error {
	root := filepath.Join(m.root, "worktrees")
	if err := m.clearUnprovisionedAllocations(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") {
			continue
		}
		m.mu.Lock()
		if _, provisioning := m.provisioning[entry.Name()]; provisioning {
			m.mu.Unlock()
			continue
		}
		if _, ok := m.agents[entry.Name()]; ok {
			_ = m.clearWorktreeAllocation(entry.Name())
			_ = m.clearWorktreeQuarantine(entry.Name())
			m.mu.Unlock()
			continue
		}
		path := filepath.Join(root, entry.Name())
		_, markerErr := os.Stat(filepath.Join(path, worktreeMarker))
		_, gitErr := os.Stat(filepath.Join(path, ".git"))
		edge, allocationErr := m.loadWorktreeAllocation(entry.Name())
		if allocationErr != nil {
			quarantineErr := m.quarantineWorktree(
				entry.Name(), path, allocationErr,
			)
			bumpNextIDLocked(m, entry.Name())
			m.mu.Unlock()
			if quarantineErr != nil {
				return fmt.Errorf(
					"quarantine unowned worktree %s: %w",
					path, quarantineErr,
				)
			}
			continue
		}
		if edge.ParentID != "" && !IsSessionParent(edge.ParentID) {
			parent, ok := m.agents[edge.ParentID]
			if !ok || parent.SessionID != edge.SessionID {
				allocationErr := errors.New(
					"worktree allocation parent is unavailable or belongs to another Session",
				)
				quarantineErr := m.quarantineWorktree(
					entry.Name(), path, allocationErr,
				)
				bumpNextIDLocked(m, entry.Name())
				m.mu.Unlock()
				if quarantineErr != nil {
					return fmt.Errorf(
						"quarantine worktree %s with invalid parent: %w",
						path, quarantineErr,
					)
				}
				continue
			}
		}
		edge.Worktree = path
		edge.Isolated = gitErr == nil && markerErr != nil
		if edge.Stance != StanceReadOnly {
			edge.ExecutionRoot = path
		}
		agent := agentFromEdge(edge)
		if err := m.recordSpawnLocked(agent); err != nil {
			m.mu.Unlock()
			return err
		}
		_ = m.clearWorktreeAllocation(agent.ID)
		_ = m.clearWorktreeQuarantine(agent.ID)
		m.agents[agent.ID] = agent
		m.worktrees[agent.ID] = &Worktree{
			ID: agent.ID, Path: path, Isolated: agent.Isolated,
		}
		reason := fmt.Sprintf(
			"worktree %s survived without a durable Agent Node", path,
		)
		result := Result{
			AgentID: agent.ID, ThreadID: agent.ThreadID,
			Status: StatusFailed, Summary: reason,
		}
		if err := m.transitionLocked(
			agent, StatusFailed, "", reason,
			"startup_reconciler", "orphaned worktree recovered", &result,
		); err != nil {
			m.mu.Unlock()
			return err
		}
		bumpNextIDLocked(m, agent.ID)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) clearUnprovisionedAllocations(worktreeRoot string) error {
	entries, err := os.ReadDir(filepath.Join(m.root, worktreeAllocations))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		agentID := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(agentID, "agent-") {
			return fmt.Errorf("invalid worktree allocation file %s", entry.Name())
		}
		_, statErr := os.Stat(filepath.Join(worktreeRoot, agentID))
		switch {
		case os.IsNotExist(statErr):
			if err := m.clearWorktreeAllocation(agentID); err != nil {
				return err
			}
		case statErr != nil:
			return statErr
		}
	}
	return nil
}
