package subagent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type IntegrationStatus string

const (
	IntegrationPreviewed IntegrationStatus = "previewed"
	IntegrationApplying  IntegrationStatus = "applying"
	IntegrationApplied   IntegrationStatus = "applied"
	IntegrationFailed    IntegrationStatus = "failed"
	IntegrationDiscarded IntegrationStatus = "discarded"
)

type IntegrationChange struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

type IntegrationReceipt struct {
	ChangedPaths []string                     `json:"changed_paths"`
	Verification protocol.ReceiptVerification `json:"verification,omitempty,omitzero"`
	AppliedAt    time.Time                    `json:"applied_at"`
}

type IntegrationCandidate struct {
	SessionID     string                       `json:"session_id"`
	AgentID       string                       `json:"agent_id"`
	AgentPath     string                       `json:"agent_path"`
	ParentID      string                       `json:"parent_id,omitempty"`
	ParentPath    string                       `json:"parent_path"`
	AttemptID     string                       `json:"attempt_id"`
	RetryOf       string                       `json:"retry_of,omitempty"`
	PreviewDigest string                       `json:"preview_digest"`
	Status        IntegrationStatus            `json:"status"`
	BaseRevision  string                       `json:"base_revision"`
	ResultTurnID  string                       `json:"result_turn_id"`
	Paths         []string                     `json:"paths"`
	Changes       []IntegrationChange          `json:"changes"`
	Conflicts     []string                     `json:"conflicts,omitempty"`
	Verification  protocol.ReceiptVerification `json:"verification,omitempty,omitzero"`
	Receipt       *IntegrationReceipt          `json:"receipt,omitempty"`
	Message       string                       `json:"message,omitempty"`
	Revision      uint64                       `json:"revision"`
	CreatedAt     time.Time                    `json:"created_at"`
	UpdatedAt     time.Time                    `json:"updated_at"`
}

func (m *Manager) SaveIntegration(candidate IntegrationCandidate) error {
	if strings.TrimSpace(candidate.AgentID) == "" ||
		strings.TrimSpace(candidate.PreviewDigest) == "" {
		return errors.New("integration agent and preview digest are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[candidate.AgentID]
	if !ok || agent.Closed {
		return errors.New("integration agent is unavailable")
	}
	if candidate.AgentPath == "" {
		candidate.AgentPath = agent.Path
	}
	if candidate.SessionID == "" {
		candidate.SessionID = agent.SessionID
	}
	if candidate.SessionID != agent.SessionID {
		return errors.New("integration candidate belongs to another session")
	}
	if candidate.ParentID == "" {
		candidate.ParentID = agent.Parent
	}
	if candidate.ParentPath == "" {
		candidate.ParentPath = agent.ParentPath
	}
	now := time.Now().UTC()
	existing, exists := m.integrations[candidate.PreviewDigest]
	if exists {
		if existing.AgentID != candidate.AgentID {
			return errors.New("integration digest belongs to another agent")
		}
		if !canTransitionIntegration(existing.Status, candidate.Status) {
			return fmt.Errorf(
				"integration %s cannot transition from %s to %s",
				candidate.PreviewDigest, existing.Status, candidate.Status,
			)
		}
		candidate.Revision = existing.Revision + 1
		candidate.CreatedAt = existing.CreatedAt
	} else {
		if candidate.Status != IntegrationPreviewed {
			return errors.New("new integration candidate must start previewed")
		}
		candidate.Revision = 1
		candidate.CreatedAt = now
	}
	candidate.UpdatedAt = now
	if err := m.recordIntegrationLocked(candidate); err != nil {
		return err
	}
	m.integrations[candidate.PreviewDigest] = cloneIntegration(candidate)
	return nil
}

func (m *Manager) Integration(
	agentID, previewDigest string,
) (IntegrationCandidate, bool, error) {
	m.mu.Lock()
	if candidate, ok := m.integrations[previewDigest]; ok {
		m.mu.Unlock()
		if candidate.AgentID != agentID {
			return IntegrationCandidate{}, false, nil
		}
		return cloneIntegration(candidate), true, nil
	}
	graph := m.graph
	sessionID := ""
	if agent, exists := m.agents[agentID]; exists {
		sessionID = agent.SessionID
	}
	m.mu.Unlock()
	if graph == nil {
		return IntegrationCandidate{}, false, nil
	}
	candidate, ok, err := graph.LoadIntegration(sessionID, agentID, previewDigest)
	if err != nil || !ok {
		return IntegrationCandidate{}, ok, err
	}
	m.mu.Lock()
	m.integrations[previewDigest] = cloneIntegration(candidate)
	m.mu.Unlock()
	return candidate, true, nil
}

func (m *Manager) BeginIntegration(agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed {
		return errors.New("integration agent is unavailable")
	}
	return m.transitionLocked(
		agent, StatusIntegrating, agent.TurnID, "integration applying",
		"parent", "integration apply started", nil,
	)
}

func (m *Manager) FinishIntegration(agentID string, applyErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed {
		return errors.New("integration agent is unavailable")
	}
	status, message := StatusIntegrated, "integration applied"
	if applyErr != nil {
		status, message = StatusIntegrationFailed, applyErr.Error()
	}
	return m.transitionLocked(
		agent, status, agent.TurnID, message,
		"parent", "integration apply finished", nil,
	)
}

func canTransitionIntegration(from, to IntegrationStatus) bool {
	switch from {
	case IntegrationPreviewed:
		return to == IntegrationApplying || to == IntegrationDiscarded
	case IntegrationApplying:
		return to == IntegrationApplied || to == IntegrationFailed
	default:
		return false
	}
}

func cloneIntegration(candidate IntegrationCandidate) IntegrationCandidate {
	candidate.Paths = append([]string(nil), candidate.Paths...)
	candidate.Changes = append([]IntegrationChange(nil), candidate.Changes...)
	candidate.Conflicts = append([]string(nil), candidate.Conflicts...)
	if candidate.Receipt != nil {
		receipt := *candidate.Receipt
		receipt.ChangedPaths = append([]string(nil), candidate.Receipt.ChangedPaths...)
		candidate.Receipt = &receipt
	}
	return candidate
}
