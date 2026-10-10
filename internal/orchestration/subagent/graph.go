package subagent

import (
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// GraphEdge is the durable Agent Node snapshot used to rebuild a tree.
type GraphEdge struct {
	ParentID       string      `json:"parent_id,omitempty"`
	ParentPath     string      `json:"parent_path"`
	ChildID        string      `json:"agent_id"`
	Path           string      `json:"path"`
	Workspace      string      `json:"workspace"`
	ExecutionRoot  string      `json:"execution_root,omitempty"`
	SessionID      string      `json:"session_id"`
	ThreadID       string      `json:"thread_id"`
	TurnID         string      `json:"turn_id,omitempty"`
	Status         Status      `json:"status"`
	Revision       uint64      `json:"revision"`
	Role           Role        `json:"role"`
	Profile        string      `json:"profile,omitempty"`
	Stance         Stance      `json:"stance"`
	Depth          int         `json:"depth"`
	Worktree       string      `json:"worktree,omitempty"`
	Isolated       bool        `json:"isolated"`
	Serialized     bool        `json:"serialized"`
	BaseRev        string      `json:"base_revision,omitempty"`
	TaskName       string      `json:"task_name,omitempty"`
	OwnedPaths     []string    `json:"owned_paths,omitempty"`
	LastMessage    string      `json:"last_message,omitempty"`
	Budget         AgentBudget `json:"budget,omitempty"`
	SpentTokens    uint64      `json:"spent_tokens,omitempty"`
	SpentMicros    uint64      `json:"spent_microunits,omitempty"`
	ReservedTokens uint64      `json:"reserved_tokens,omitempty"`
	ReservedMicros uint64      `json:"reserved_microunits,omitempty"`
}

type CompletionEnvelope struct {
	AgentPath        string                       `json:"agent_path"`
	Status           Status                       `json:"status"`
	Summary          string                       `json:"summary,omitempty"`
	ResultRef        string                       `json:"result_ref"`
	ReceiptRef       string                       `json:"receipt_ref,omitempty"`
	ChangedPaths     []string                     `json:"changed_paths,omitempty"`
	Verification     protocol.ReceiptVerification `json:"verification,omitempty,omitzero"`
	Usage            ResultUsage                  `json:"usage"`
	IntegrationReady bool                         `json:"integration_ready"`
}

type GraphTransition struct {
	SessionID         string              `json:"session_id"`
	AgentID           string              `json:"agent_id"`
	Path              string              `json:"path"`
	ExpectedRevision  uint64              `json:"expected_revision"`
	Status            Status              `json:"status"`
	TurnID            string              `json:"turn_id,omitempty"`
	Message           string              `json:"message,omitempty"`
	OperationID       string              `json:"operation_id"`
	Actor             string              `json:"actor"`
	Reason            string              `json:"reason,omitempty"`
	ReasonCode        string              `json:"reason_code,omitempty"`
	Result            *Result             `json:"result,omitempty"`
	Completion        *CompletionEnvelope `json:"completion,omitempty"`
	CompletionMessage *Message            `json:"completion_message,omitempty"`
	ReserveTokens     uint64              `json:"reserve_tokens,omitempty"`
	ReserveMicros     uint64              `json:"reserve_microunits,omitempty"`
	ReleaseBudget     bool                `json:"release_budget,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
}

type BudgetLedger struct {
	ReservedTokens uint64 `json:"reserved_tokens"`
	SpentTokens    uint64 `json:"spent_tokens"`
	ReservedMicros uint64 `json:"reserved_microunits"`
	SpentMicros    uint64 `json:"spent_microunits"`
	ReservedSlots  int    `json:"reserved_slots"`
	TotalSpawned   int    `json:"total_spawned"`
}

// Graph is the durable Agent Node, Mailbox, Result, and Budget store.
type Graph interface {
	RecordSpawn(edge GraphEdge) error
	RecordTransition(transition GraphTransition) error
	RecordMessage(message Message) error
	MarkDelivered(message Message) error
	ListSessions() ([]string, error)
	ListChildren(sessionID, parentID string) ([]GraphEdge, error)
	ListMessages(sessionID, to string) ([]Message, error)
	LoadResult(sessionID, agentID string) (Result, bool, error)
	LoadIntegrationResult(sessionID, agentID string) (Result, bool, error)
	RecordIntegration(candidate IntegrationCandidate) error
	LoadIntegration(sessionID, agentID, previewDigest string) (IntegrationCandidate, bool, error)
	Reconcile() error
}

type graphIdentity interface {
	AgentIdentity() (workspace, sessionID string)
}

// AttachGraph installs durable topology and hydrates in-memory agents from it.
func (m *Manager) AttachGraph(graph Graph) error {
	if graph == nil {
		return nil
	}
	m.mu.Lock()
	m.graph = graph
	if identity, ok := graph.(graphIdentity); ok {
		m.workspace, m.sessionID = identity.AgentIdentity()
		m.mailbox.mu.Lock()
		m.mailbox.defaultSession = m.sessionID
		m.mailbox.mu.Unlock()
	}
	m.mu.Unlock()
	if err := graph.Reconcile(); err != nil {
		return err
	}
	return m.Hydrate()
}

// Hydrate loads projected children into the in-memory map (mailbox untouched).
func (m *Manager) Hydrate() error {
	m.mu.Lock()
	graph := m.graph
	m.mu.Unlock()
	if graph == nil {
		return nil
	}
	sessions, err := graph.ListSessions()
	if err != nil {
		return err
	}
	for _, sessionID := range sessions {
		// Breadth-first from the session parent actor. "" remains for
		// historical edges written before ParentID was bound to "parent".
		seen := map[string]struct{}{"": {}, SessionParentID: {}}
		queue := []string{"", SessionParentID}
		for len(queue) > 0 {
			parent := queue[0]
			queue = queue[1:]
			edges, err := graph.ListChildren(sessionID, parent)
			if err != nil {
				return err
			}
			for _, edge := range edges {
				result, settled, err := graph.LoadResult(sessionID, edge.ChildID)
				if err != nil {
					return err
				}
				integrationResult, integrationReady, err :=
					graph.LoadIntegrationResult(sessionID, edge.ChildID)
				if err != nil {
					return err
				}
				m.mu.Lock()
				if _, ok := m.agents[edge.ChildID]; !ok {
					m.agents[edge.ChildID] = agentFromEdge(edge)
				}
				if edge.Worktree != "" && edge.Stance != StanceReadOnly {
					m.worktrees[edge.ChildID] = &Worktree{
						ID: edge.ChildID, Path: edge.Worktree, Isolated: edge.Isolated,
						Serialized: edge.Serialized, BaseRev: edge.BaseRev,
					}
				}
				if settled {
					m.agents[edge.ChildID].Result = &result
				}
				if integrationReady {
					m.agents[edge.ChildID].IntegrationResult = &integrationResult
					if !m.agents[edge.ChildID].Closed {
						m.claimLocked(edge.ChildID, integrationResult.WritePaths())
					}
				}
				bumpNextIDLocked(m, edge.ChildID)
				if _, ok := seen[edge.ChildID]; !ok {
					seen[edge.ChildID] = struct{}{}
					queue = append(queue, edge.ChildID)
				}
				m.mu.Unlock()
			}
		}
		messages, err := graph.ListMessages(sessionID, "")
		if err != nil {
			return err
		}
		m.mailbox.Restore(messages)
	}
	return m.reconcileOrphanWorktrees()
}

func bumpNextIDLocked(m *Manager, id string) {
	var n int
	if _, err := fmt.Sscanf(id, "agent-%d", &n); err == nil && n > m.nextID {
		m.nextID = n
	}
}

func agentFromEdge(edge GraphEdge) *Agent {
	return &Agent{
		ID: edge.ChildID, Path: edge.Path, Revision: edge.Revision,
		Role: edge.Role, Profile: edge.Profile, Stance: edge.Stance,
		Depth: edge.Depth, Worktree: edge.Worktree, Parent: edge.ParentID,
		Isolated: edge.Isolated, Serialized: edge.Serialized,
		ParentPath: edge.ParentPath, Workspace: edge.Workspace, SessionID: edge.SessionID,
		ExecutionRoot: edge.ExecutionRoot,
		ThreadID:      edge.ThreadID, TurnID: edge.TurnID, BaseRev: edge.BaseRev,
		TaskName: edge.TaskName, Closed: edge.Status == StatusClosed, Status: edge.Status,
		LastMessage: edge.LastMessage,
		OwnedPaths:  append([]string(nil), edge.OwnedPaths...),
		Budget:      edge.Budget, SpentTokens: edge.SpentTokens,
		SpentMicros: edge.SpentMicros, ReservedTokens: edge.ReservedTokens,
		ReservedMicros: edge.ReservedMicros,
	}
}

func (m *Manager) recordSpawnLocked(agent *Agent) error {
	if m.graph == nil || agent == nil {
		return nil
	}
	return m.graph.RecordSpawn(GraphEdge{
		ParentID: agent.Parent, ParentPath: agent.ParentPath,
		ChildID: agent.ID, Path: agent.Path, Status: agent.Status,
		Workspace: agent.Workspace, SessionID: agent.SessionID,
		ExecutionRoot: agent.ExecutionRoot,
		ThreadID:      agent.ThreadID, TurnID: agent.TurnID, Revision: agent.Revision,
		Role: agent.Role, Profile: agent.Profile, Stance: agent.Stance,
		Depth: agent.Depth, Worktree: agent.Worktree,
		Isolated: agent.Isolated, Serialized: agent.Serialized, BaseRev: agent.BaseRev,
		TaskName:   agent.TaskName,
		OwnedPaths: append([]string(nil), agent.OwnedPaths...),
		Budget:     agent.Budget, SpentTokens: agent.SpentTokens,
		SpentMicros: agent.SpentMicros, ReservedTokens: agent.ReservedTokens,
		ReservedMicros: agent.ReservedMicros,
	})
}

func (m *Manager) recordTransitionLocked(transition GraphTransition) error {
	if m.graph == nil {
		return nil
	}
	return m.graph.RecordTransition(transition)
}

func (m *Manager) recordMessage(message Message) error {
	m.mu.Lock()
	graph := m.graph
	m.mu.Unlock()
	if graph == nil {
		return nil
	}
	return graph.RecordMessage(message)
}

func (m *Manager) recordDelivery(message Message) error {
	m.mu.Lock()
	graph := m.graph
	m.mu.Unlock()
	if graph == nil {
		return nil
	}
	return graph.MarkDelivered(message)
}

// DurableGraph adapts a recorder that can append agent protocol events and list edges.
type DurableGraph struct {
	Workspace         string
	SessionID         string
	AppendSpawn       func(GraphEdge) error
	AppendStatus      func(GraphTransition) error
	AppendMessage     func(Message) error
	DeliverMessage    func(Message) error
	Sessions          func() ([]string, error)
	Children          func(sessionID, parentID string) ([]GraphEdge, error)
	Messages          func(sessionID, to string) ([]Message, error)
	Result            func(sessionID, agentID string) (Result, bool, error)
	IntegrationResult func(sessionID, agentID string) (Result, bool, error)
	AppendIntegration func(IntegrationCandidate) error
	Integration       func(sessionID, agentID, previewDigest string) (IntegrationCandidate, bool, error)
	ReconcileGraph    func() error
}

func (g DurableGraph) AgentIdentity() (string, string) {
	return g.Workspace, g.SessionID
}

func (g DurableGraph) RecordSpawn(edge GraphEdge) error {
	if g.AppendSpawn == nil {
		return fmt.Errorf("agent graph spawn recorder is required")
	}
	return g.AppendSpawn(edge)
}

func (g DurableGraph) RecordTransition(transition GraphTransition) error {
	if g.AppendStatus == nil {
		return fmt.Errorf("agent graph status recorder is required")
	}
	return g.AppendStatus(transition)
}

func (g DurableGraph) RecordMessage(message Message) error {
	if g.AppendMessage == nil {
		return fmt.Errorf("agent graph message recorder is required")
	}
	return g.AppendMessage(message)
}

func (g DurableGraph) MarkDelivered(message Message) error {
	if g.DeliverMessage == nil {
		return fmt.Errorf("agent graph delivery recorder is required")
	}
	return g.DeliverMessage(message)
}

func (g DurableGraph) ListSessions() ([]string, error) {
	if g.Sessions == nil {
		return nil, fmt.Errorf("agent graph session list is required")
	}
	return g.Sessions()
}

func (g DurableGraph) ListChildren(sessionID, parentID string) ([]GraphEdge, error) {
	if g.Children == nil {
		return nil, fmt.Errorf("agent graph list is required")
	}
	return g.Children(sessionID, parentID)
}

func (g DurableGraph) ListMessages(sessionID, to string) ([]Message, error) {
	if g.Messages == nil {
		return nil, fmt.Errorf("agent graph mailbox list is required")
	}
	return g.Messages(sessionID, to)
}

func (g DurableGraph) LoadResult(sessionID, agentID string) (Result, bool, error) {
	if g.Result == nil {
		return Result{}, false, fmt.Errorf("agent graph result loader is required")
	}
	return g.Result(sessionID, agentID)
}

func (g DurableGraph) LoadIntegrationResult(
	sessionID, agentID string,
) (Result, bool, error) {
	if g.IntegrationResult == nil {
		return Result{}, false, nil
	}
	return g.IntegrationResult(sessionID, agentID)
}

func (g DurableGraph) RecordIntegration(candidate IntegrationCandidate) error {
	if g.AppendIntegration == nil {
		return fmt.Errorf("agent graph integration recorder is required")
	}
	return g.AppendIntegration(candidate)
}

func (g DurableGraph) LoadIntegration(
	sessionID, agentID, previewDigest string,
) (IntegrationCandidate, bool, error) {
	if g.Integration == nil {
		return IntegrationCandidate{}, false,
			fmt.Errorf("agent graph integration loader is required")
	}
	return g.Integration(sessionID, agentID, previewDigest)
}

func (g DurableGraph) Reconcile() error {
	if g.ReconcileGraph == nil {
		return nil
	}
	return g.ReconcileGraph()
}

func (m *Manager) recordIntegrationLocked(candidate IntegrationCandidate) error {
	if m.graph == nil {
		return nil
	}
	return m.graph.RecordIntegration(candidate)
}
