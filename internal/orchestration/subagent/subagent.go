// Package subagent owns role routing, worktree isolation, mailbox, and takeover.
package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type Role string

// Canonical roles aligned with the reference SUBAGENTS taxonomy.
const (
	RoleGeneral     Role = "general"
	RoleExplore     Role = "explore"
	RolePlan        Role = "plan"
	RoleReview      Role = "review"
	RoleImplementer Role = "implementer"
	RoleVerifier    Role = "verifier"
	RoleAwaiter     Role = "awaiter"
	RoleCustom      Role = "custom"
)

type Stance string

const (
	StanceWrite        Stance = "write"
	StanceReadOnly     Stance = "read_only"
	StanceMinimalWrite Stance = "minimal_write"
	StanceTestFocused  Stance = "test_focused"
	StanceCustom       Stance = "custom"
)

type Budget struct {
	MaxSteps    int
	MaxTokens   uint64
	MaxCostUSD  float64
	MaxDepth    int
	MaxParallel int
	MaxResident int
	MaxTotal    int
}

func (b Budget) WithDefaults() Budget {
	if b.MaxDepth <= 0 {
		b.MaxDepth = 5
	}
	if b.MaxParallel <= 0 {
		b.MaxParallel = 8
	}
	if b.MaxResident <= 0 {
		b.MaxResident = 8
	}
	if b.MaxResident < b.MaxParallel {
		b.MaxResident = b.MaxParallel
	}
	if b.MaxTotal <= 0 {
		b.MaxTotal = 16
	}
	if b.MaxTotal < b.MaxResident {
		b.MaxTotal = b.MaxResident
	}
	return b
}

type AgentBudget struct {
	MaxSteps   int     `json:"max_steps,omitempty"`
	MaxTokens  uint64  `json:"max_tokens,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

type Route struct {
	Role    Role
	Profile string
	Stance  Stance
}

// ParseRole maps canonical names and compatibility aliases to a Role.
func ParseRole(raw string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "general", "worker":
		return RoleGeneral, nil
	case "explore", "explorer", "exploration":
		return RoleExplore, nil
	case "plan", "planner":
		return RolePlan, nil
	case "review", "reviewer":
		return RoleReview, nil
	case "implementer", "implement", "implementation", "builder":
		return RoleImplementer, nil
	case "verifier", "verify", "verification", "validator", "tester":
		return RoleVerifier, nil
	case "awaiter", "await":
		return RoleAwaiter, nil
	case "custom":
		return RoleCustom, nil
	default:
		return "", fmt.Errorf("unsupported role %q", raw)
	}
}

type ToolGate interface {
	Execute(ctx context.Context, callID, name string, raw json.RawMessage) (tool.Result, error)
}

type RuntimeHost interface {
	StartTurn(ctx context.Context, agentID, prompt string) (string, error)
	CancelTurn(ctx context.Context, agentID, turnID string) error
}

type Options struct {
	Root      string
	Workspace string
	SessionID string
	Budget    Budget
	Gate      ToolGate
	Runtime   RuntimeHost
	Worktrees WorktreeProvider
	Roles     RoleCatalog
}

type Manager struct {
	mu            sync.Mutex
	wait          *sync.Cond
	root          string
	budget        Budget
	gate          ToolGate
	runtime       RuntimeHost
	trees         WorktreeProvider
	roles         RoleCatalog
	agents        map[string]*Agent
	mailbox       *Mailbox
	worktrees     map[string]*Worktree
	claims        map[string]string // workspace-relative path -> owning agent id
	integrations  map[string]IntegrationCandidate
	active        map[string]int
	graph         Graph
	workspace     string
	sessionID     string
	nextID        int
	ledgers       map[string]BudgetLedger
	nextExecution uint64
	executions    map[string]map[uint64]context.CancelFunc
	closing       map[string]*agentCloseState
	starting      map[string]bool
}

type agentCloseState struct {
	done chan struct{}
	err  error
}

type Agent struct {
	ID            string
	Path          string
	ParentPath    string
	Revision      uint64
	Workspace     string
	ExecutionRoot string
	SessionID     string
	ThreadID      string
	Role          Role
	Profile       string
	Stance        Stance
	Depth         int
	// Worktree is where this agent works, and Isolated says whether writing
	// there is safe. A writing agent without isolation must not run.
	Worktree string
	Isolated bool
	// Serialized means this child deliberately shares the host workspace and
	// may run only while holding the session's whole-turn workspace gate.
	Serialized bool
	// BaseRev is the spawn-time git revision of an isolated worktree; empty for
	// scratch / read-only children that never get a checkout.
	BaseRev           string
	Parent            string
	Closed            bool
	Status            Status
	TurnID            string
	LastMessage       string
	Result            *Result
	IntegrationResult *Result
	TaskName          string
	ExpectedOutput    string
	OwnedPaths        []string
	DelegationTrigger DelegationTrigger
	TraceParent       string
	TraceState        string
	RoleInstructions  string
	Context           *ContextReceipt
	Budget            AgentBudget
	SpentTokens       uint64
	SpentMicros       uint64
	ReservedTokens    uint64
	ReservedMicros    uint64
	Resident          bool
	LastActiveAt      time.Time
}

func Open(options Options) (*Manager, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("subagent root is required")
	}
	if options.Gate == nil {
		return nil, errors.New("tool gate is required")
	}
	if err := os.MkdirAll(options.Root, 0o700); err != nil {
		return nil, err
	}
	trees := options.Worktrees
	if trees == nil {
		trees = scratchWorktrees{root: options.Root}
	}
	roles := options.Roles
	if len(roles.specs) == 0 {
		roles = DefaultRoleCatalog()
	}
	manager := &Manager{
		root: options.Root, budget: options.Budget.WithDefaults(),
		gate: options.Gate, runtime: options.Runtime, trees: trees,
		workspace: options.Workspace, sessionID: options.SessionID,
		roles:  roles,
		agents: make(map[string]*Agent), mailbox: NewMailbox(),
		worktrees:    make(map[string]*Worktree),
		claims:       make(map[string]string),
		integrations: make(map[string]IntegrationCandidate),
		active:       make(map[string]int),
		ledgers:      make(map[string]BudgetLedger),
		executions:   make(map[string]map[uint64]context.CancelFunc),
		closing:      make(map[string]*agentCloseState),
		starting:     make(map[string]bool),
	}
	if manager.sessionID == "" {
		manager.sessionID = "session-local"
	}
	if manager.workspace == "" {
		manager.workspace = options.Root
	}
	manager.mailbox.defaultSession = manager.sessionID
	manager.wait = sync.NewCond(&manager.mu)
	manager.mailbox.persist = manager.recordMessage
	manager.mailbox.deliver = manager.recordDelivery
	return manager, nil
}

func (m *Manager) Route(role Role) Route {
	spec, err := m.roles.Resolve(role)
	if err != nil {
		spec, _ = m.roles.Resolve(RoleGeneral)
	}
	return Route{Role: spec.Role, Profile: spec.Profile, Stance: spec.Stance}
}

// Spawn is retained for internal compatibility. Model-facing and worker paths
// use AgentControl, which adds delegation admission and structured intent.
func (m *Manager) Spawn(parentID string, role Role, prompt string) (*Agent, error) {
	spec, err := m.roles.Resolve(role)
	if err != nil {
		return nil, err
	}
	return m.spawn(DelegationIntent{
		TaskName:       "internal_task",
		Role:           role,
		Objective:      prompt,
		ExpectedOutput: "Return a concise result with supporting evidence.",
		ParentID:       parentID,
		Trigger:        TriggerSystem,
	}, spec)
}

func (m *Manager) spawn(intent DelegationIntent, spec RoleSpec) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID := strings.TrimSpace(intent.SessionID)
	if sessionID == "" {
		sessionID = m.sessionID
	}
	if sessionID == "" {
		return nil, errors.New("subagent session id is required")
	}
	intent.ParentID = BindSessionParent(intent.ParentID)
	depth := 0
	parentPath := "/root"
	executionRoot := m.workspace
	var parent *Agent
	if !IsSessionParent(intent.ParentID) {
		var ok bool
		parent, ok = m.agents[intent.ParentID]
		if !ok || parent.Closed || m.closing[parent.ID] != nil {
			return nil, errors.New("parent agent unavailable")
		}
		if parent.SessionID != sessionID {
			return nil, errors.New("parent agent belongs to another session")
		}
		depth = parent.Depth + 1
		parentPath = parent.Path
	}
	if depth > m.budget.MaxDepth {
		return nil, fmt.Errorf("recursion depth %d exceeds limit %d", depth, m.budget.MaxDepth)
	}
	ledger := m.ledgers[sessionID]
	if ledger.TotalSpawned >= m.budget.MaxTotal {
		return nil, errors.New("subagent total spawn budget exhausted")
	}
	requested, err := m.normalizeAgentBudgetLocked(
		intent.Budget, spec.DefaultBudget, parent,
	)
	if err != nil {
		return nil, err
	}
	if intent.Budget.MaxTokens == 0 &&
		spec.DefaultBudget.MaxTokens == 0 &&
		m.budget.MaxTokens > 0 {
		remaining := m.budget.MaxTokens -
			min(m.budget.MaxTokens, ledger.SpentTokens+ledger.ReservedTokens)
		if remaining == 0 {
			return nil, errors.New("subagent token tree budget exhausted")
		}
		requested.MaxTokens = min(requested.MaxTokens, remaining)
	}
	reservedMicros := uint64(requested.MaxCostUSD * 1e6)
	if m.budget.MaxTokens > 0 &&
		ledger.SpentTokens+ledger.ReservedTokens+requested.MaxTokens >
			m.budget.MaxTokens {
		return nil, errors.New("subagent token reservation exceeds tree budget")
	}
	maxMicros := uint64(m.budget.MaxCostUSD * 1e6)
	if maxMicros > 0 &&
		ledger.SpentMicros+ledger.ReservedMicros+reservedMicros >
			maxMicros {
		return nil, errors.New("subagent cost reservation exceeds tree budget")
	}
	m.nextID++
	id := fmt.Sprintf("agent-%d", m.nextID)
	path := m.nextPathLocked(intent.ParentID, intent.TaskName, id)
	threadID := ThreadIDFor(id)
	allocation := GraphEdge{
		ParentID: intent.ParentID, ParentPath: parentPath,
		ChildID: id, Path: path, Status: StatusRequested,
		Workspace: m.workspace, SessionID: sessionID,
		ExecutionRoot: executionRoot, ThreadID: threadID, Revision: 1,
		Role: spec.Role, Profile: spec.Profile, Stance: spec.Stance,
		Depth: depth, TaskName: strings.TrimSpace(intent.TaskName),
		OwnedPaths: append([]string(nil), intent.OwnedPaths...),
		Budget:     requested,
	}
	if spec.Stance == StanceReadOnly && parent != nil &&
		strings.TrimSpace(parent.Worktree) != "" {
		executionRoot = parent.Worktree
	}
	var wt Worktree
	owned := false
	if spec.Stance == StanceReadOnly {
		// Read-only children share the host (or parent) workspace. A scratch or
		// git worktree would be unused isolation cost and a leftover directory.
		wt = Worktree{ID: id, Path: executionRoot}
	} else {
		if err := m.recordWorktreeAllocation(allocation); err != nil {
			return nil, fmt.Errorf("record worktree allocation: %w", err)
		}
		// The stance decides what kind of directory the agent needs, so routing has
		// to happen before provisioning: an explore child must not pay for a checkout.
		provisioned, err := m.trees.Provision(id, spec.Stance)
		if err != nil {
			m.clearAllocationWithoutWorktree(id)
			return nil, err
		}
		wt = provisioned
		owned = true
		if wt.Serialized {
			_ = m.clearWorktreeAllocation(id)
		}
		executionRoot = wt.Path
	}
	agent := &Agent{
		ID: id, Path: path,
		ParentPath: parentPath,
		Revision:   1, Workspace: m.workspace, ExecutionRoot: executionRoot,
		SessionID: sessionID,
		ThreadID:  threadID,
		Role:      spec.Role, Profile: spec.Profile, Stance: spec.Stance,
		Depth: depth, Worktree: wt.Path, Isolated: wt.Isolated,
		Serialized: wt.Serialized, BaseRev: wt.BaseRev,
		Parent: intent.ParentID, Status: StatusRequested,
		TaskName:          strings.TrimSpace(intent.TaskName),
		ExpectedOutput:    strings.TrimSpace(intent.ExpectedOutput),
		OwnedPaths:        append([]string(nil), intent.OwnedPaths...),
		DelegationTrigger: intent.Trigger,
		TraceParent:       intent.TraceParent, TraceState: intent.TraceState,
		RoleInstructions: spec.Instructions,
		Budget:           requested,
	}
	if err := m.recordSpawnLocked(agent); err != nil {
		var discardErr error
		if owned {
			discardErr = m.trees.Discard(wt)
			if discardErr == nil {
				_ = m.clearWorktreeAllocation(id)
			}
		}
		return nil, errors.Join(
			fmt.Errorf("record agent spawn: %w", err),
			discardErr,
		)
	}
	_ = m.clearWorktreeAllocation(id)
	m.agents[id] = agent
	if owned {
		m.worktrees[id] = &wt
	}
	ledger.TotalSpawned++
	m.ledgers[sessionID] = ledger
	m.wait.Broadcast()
	return agent, nil
}

func (m *Manager) normalizeAgentBudgetLocked(
	requested AgentBudget,
	role Budget,
	parent *Agent,
) (AgentBudget, error) {
	if requested.MaxSteps == 0 {
		requested.MaxSteps = role.MaxSteps
	}
	if requested.MaxSteps == 0 {
		requested.MaxSteps = m.budget.MaxSteps
	}
	if requested.MaxTokens == 0 {
		requested.MaxTokens = role.MaxTokens
	}
	if requested.MaxTokens == 0 {
		requested.MaxTokens = parallelBudgetShare(
			m.budget.MaxTokens,
			m.budget.MaxParallel,
		)
	}
	if requested.MaxCostUSD == 0 {
		requested.MaxCostUSD = role.MaxCostUSD
	}
	if requested.MaxCostUSD == 0 {
		requested.MaxCostUSD = m.budget.MaxCostUSD /
			float64(max(1, m.budget.MaxParallel))
	}
	if m.budget.MaxSteps > 0 && requested.MaxSteps > m.budget.MaxSteps {
		return AgentBudget{}, errors.New("child step budget exceeds tree ceiling")
	}
	if parent != nil {
		if parent.Budget.MaxSteps > 0 &&
			requested.MaxSteps > parent.Budget.MaxSteps {
			return AgentBudget{}, errors.New("child step budget exceeds parent ceiling")
		}
		if parent.Budget.MaxTokens > 0 &&
			requested.MaxTokens > parent.Budget.MaxTokens {
			return AgentBudget{}, errors.New("child token budget exceeds parent ceiling")
		}
		if parent.Budget.MaxCostUSD > 0 &&
			requested.MaxCostUSD > parent.Budget.MaxCostUSD {
			return AgentBudget{}, errors.New("child cost budget exceeds parent ceiling")
		}
	}
	return requested, nil
}

func parallelBudgetShare(total uint64, parallel int) uint64 {
	if total == 0 {
		return 0
	}
	return max(uint64(1), total/uint64(max(1, parallel)))
}

func (m *Manager) residentLocked(sessionID string) int {
	resident := 0
	for _, agent := range m.agents {
		if agent != nil && agent.SessionID == sessionID &&
			!agent.Closed && agent.Status != StatusClosed && agent.Resident {
			resident++
		}
	}
	return resident
}

// ActivateResident marks an Agent's Runtime Thread resident and returns any
// terminal LRU Agents that must be unloaded first.
func (m *Manager) ActivateResident(agentID string) ([]Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusClosed {
		return nil, errors.New("agent not found")
	}
	now := time.Now().UTC()
	if agent.Resident {
		agent.LastActiveAt = now
		return nil, nil
	}
	var evicted []Agent
	if m.residentLocked(agent.SessionID) >= m.budget.MaxResident {
		var candidate *Agent
		for _, current := range m.agents {
			if current == nil || current.ID == agentID ||
				current.SessionID != agent.SessionID || !current.Resident ||
				current.Closed || occupiesSlot(current.Status) {
				continue
			}
			if candidate == nil ||
				current.LastActiveAt.Before(candidate.LastActiveAt) ||
				(current.LastActiveAt.Equal(candidate.LastActiveAt) &&
					current.ID < candidate.ID) {
				candidate = current
			}
		}
		if candidate == nil {
			return nil, errors.New("subagent resident budget exhausted")
		}
		candidate.Resident = false
		evicted = append(evicted, cloneAgent(candidate))
	}
	agent.Resident = true
	agent.LastActiveAt = now
	return evicted, nil
}

func (m *Manager) DeactivateResident(agentID string) {
	m.mu.Lock()
	if agent := m.agents[agentID]; agent != nil {
		agent.Resident = false
	}
	m.mu.Unlock()
}

func (m *Manager) TouchResident(agentID string) {
	m.mu.Lock()
	if agent := m.agents[agentID]; agent != nil && agent.Resident {
		agent.LastActiveAt = time.Now().UTC()
	}
	m.mu.Unlock()
}

func (m *Manager) ExecuteTool(ctx context.Context, agentID, callID, name string, raw json.RawMessage) (tool.Result, error) {
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	gate := m.gate
	if !ok || agent.Closed || m.closing[agentID] != nil {
		m.mu.Unlock()
		return tool.Result{}, errors.New("agent not found")
	}
	if gate == nil {
		m.mu.Unlock()
		return tool.Result{}, errors.New("tool gate unavailable")
	}
	executionContext, cancel := context.WithCancel(ctx)
	m.nextExecution++
	executionID := m.nextExecution
	if m.executions[agentID] == nil {
		m.executions[agentID] = make(map[uint64]context.CancelFunc)
	}
	m.executions[agentID][executionID] = cancel
	m.mu.Unlock()
	defer m.releaseExecution(agentID, executionID, cancel)
	return gate.Execute(executionContext, callID, name, raw)
}

func (m *Manager) releaseExecution(
	agentID string,
	executionID uint64,
	cancel context.CancelFunc,
) {
	cancel()
	m.mu.Lock()
	if executions := m.executions[agentID]; executions != nil {
		delete(executions, executionID)
		if len(executions) == 0 {
			delete(m.executions, agentID)
		}
	}
	m.wait.Broadcast()
	m.mu.Unlock()
}

func (m *Manager) Takeover(ctx context.Context, agentID, prompt string) (string, error) {
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusShutdown || m.closing[agentID] != nil {
		m.mu.Unlock()
		return "", errors.New("agent not found")
	}
	if agent.Status == StatusRunning {
		m.mu.Unlock()
		return "", errors.New("agent is busy")
	}
	runtime := m.runtime
	m.mu.Unlock()
	return m.startTurn(ctx, agentID, prompt, runtime)
}

func (m *Manager) Mailbox() *Mailbox { return m.mailbox }

// Agent returns a snapshot of an open agent, or false if missing/closed.
func (m *Manager) Agent(id string) (Agent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[id]
	if !ok || agent.Closed {
		return Agent{}, false
	}
	return cloneAgent(agent), true
}

func (m *Manager) AgentByThread(threadID string) (Agent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		if agent != nil && !agent.Closed && agent.ThreadID == threadID {
			return cloneAgent(agent), true
		}
	}
	return Agent{}, false
}

func (m *Manager) AgentSession(agentID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent == nil {
		return "", false
	}
	return agent.SessionID, true
}

func (m *Manager) IsDescendant(parentID, agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.agents[agentID]
	if current == nil {
		return false
	}
	if IsSessionParent(parentID) && IsSessionParent(current.Parent) {
		return true
	}
	for current != nil && current.Parent != "" {
		if current.Parent == parentID ||
			(IsSessionParent(parentID) && IsSessionParent(current.Parent)) {
			return true
		}
		current = m.agents[current.Parent]
	}
	return false
}

func cloneAgent(agent *Agent) Agent {
	if agent == nil {
		return Agent{}
	}
	cloned := *agent
	cloned.OwnedPaths = append([]string(nil), agent.OwnedPaths...)
	if agent.Context != nil {
		context := cloneContextReceipt(*agent.Context)
		cloned.Context = &context
	}
	if agent.Result != nil {
		result := *agent.Result
		cloned.Result = &result
	}
	if agent.IntegrationResult != nil {
		result := *agent.IntegrationResult
		cloned.IntegrationResult = &result
	}
	return cloned
}

func (m *Manager) Close(agentID string) error {
	return m.CloseContext(context.Background(), agentID)
}

// CloseContext fences new work, waits for the real turn settlement, then closes
// the agent. A failed or canceled request leaves the agent available for retry.
func (m *Manager) CloseContext(ctx context.Context, agentID string) error {
	wake := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		m.wait.Broadcast()
		m.mu.Unlock()
	})
	defer wake()
	m.mu.Lock()
	if state := m.closing[agentID]; state != nil {
		m.mu.Unlock()
		select {
		case <-state.done:
			return state.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	state := &agentCloseState{done: make(chan struct{})}
	m.closing[agentID] = state
	agent, ok := m.agents[agentID]
	if !ok {
		m.finishCloseLocked(agentID, state, errors.New("agent not found"))
		m.mu.Unlock()
		return state.err
	}
	if agent.Closed {
		m.finishCloseLocked(agentID, state, nil)
		m.mu.Unlock()
		return nil
	}
	cancels := make([]context.CancelFunc, 0, len(m.executions[agentID]))
	for _, cancel := range m.executions[agentID] {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	m.mu.Lock()
	// StartTurn submits outside the Manager lock. Let it publish the accepted
	// turn identity before canceling, including when its terminal arrived early.
	for m.starting[agentID] && ctx.Err() == nil {
		m.wait.Wait()
	}
	if err := ctx.Err(); err != nil {
		m.finishCloseLocked(agentID, state, err)
		m.mu.Unlock()
		return err
	}
	if occupiesSlot(agent.Status) {
		m.mu.Unlock()
		_, err := m.Interrupt(ctx, agentID)
		m.mu.Lock()
		if err != nil {
			m.finishCloseLocked(agentID, state, err)
			m.mu.Unlock()
			return err
		}
		for occupiesSlot(agent.Status) && ctx.Err() == nil {
			m.wait.Wait()
		}
		if err := ctx.Err(); err != nil {
			m.finishCloseLocked(agentID, state, err)
			m.mu.Unlock()
			return err
		}
	}
	if err := m.transitionLocked(
		agent, StatusClosed, agent.TurnID, agent.LastMessage,
		"parent", "agent closed", nil,
	); err != nil {
		m.finishCloseLocked(agentID, state, err)
		m.mu.Unlock()
		return err
	}
	agent.Closed = true
	agent.Resident = false
	m.releaseClaimsLocked(agentID)
	wt := m.worktrees[agentID]
	var worktree *Worktree
	var closeErr error
	if wt != nil {
		if err := m.checkWorktreeOverlapLocked(wt); err != nil {
			closeErr = err
		} else {
			copy := *wt
			worktree = &copy
		}
	}
	for len(m.executions[agentID]) > 0 {
		m.wait.Wait()
	}
	m.mu.Unlock()

	if closeErr == nil && worktree != nil {
		closeErr = m.trees.Discard(*worktree)
	}
	m.mu.Lock()
	m.finishCloseLocked(agentID, state, closeErr)
	m.mu.Unlock()
	return closeErr
}

func (m *Manager) finishCloseLocked(
	agentID string,
	state *agentCloseState,
	err error,
) {
	state.err = err
	delete(m.closing, agentID)
	close(state.done)
}

type Message struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	Sequence    uint64          `json:"sequence"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	Kind        MessageKind     `json:"kind"`
	PayloadRef  string          `json:"payload_ref,omitempty"`
	Body        json.RawMessage `json:"body"`
	TriggerTurn bool            `json:"trigger_turn,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
}

type MessageKind string

const (
	MessageContext     MessageKind = "context"
	MessageTask        MessageKind = "task"
	MessageCompletion  MessageKind = "completion"
	MessageInterrupt   MessageKind = "interrupt"
	MessageIntegration MessageKind = "integration"
)

type Mailbox struct {
	mu             sync.Mutex
	writeGate      chan struct{}
	seq            map[string]uint64
	pending        []Message
	closed         bool
	defaultSession string
	persist        func(Message) error
	deliver        func(Message) error
}

func NewMailbox() *Mailbox {
	mailbox := &Mailbox{
		seq:       make(map[string]uint64),
		writeGate: make(chan struct{}, 1),
	}
	mailbox.writeGate <- struct{}{}
	return mailbox
}

func (m *Mailbox) Close() {
	<-m.writeGate
	defer func() { m.writeGate <- struct{}{} }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
}

func (m *Mailbox) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Mailbox) Deliver(from, to string, body json.RawMessage) (Message, error) {
	return m.Enqueue(Message{
		From: from, To: to, Kind: MessageContext, Body: body,
	})
}

func (m *Mailbox) Enqueue(message Message) (Message, error) {
	<-m.writeGate
	defer func() { m.writeGate <- struct{}{} }()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Message{}, errors.New("mailbox closed")
	}
	if strings.TrimSpace(message.To) == "" {
		m.mu.Unlock()
		return Message{}, errors.New("mailbox target is required")
	}
	if len(message.Body) == 0 || len(message.Body) > 16<<10 ||
		!json.Valid(message.Body) {
		m.mu.Unlock()
		return Message{}, errors.New("mailbox body must be bounded JSON")
	}
	message = m.prepareLocked(message)
	persist := m.persist
	m.mu.Unlock()
	if persist != nil {
		if err := persist(message); err != nil {
			return Message{}, err
		}
	}
	m.mu.Lock()
	m.pending = append(m.pending, message)
	m.mu.Unlock()
	return message, nil
}

func (m *Mailbox) Prepare(message Message) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || strings.TrimSpace(message.To) == "" {
		return Message{}, errors.New("mailbox is closed or target is missing")
	}
	return m.prepareLocked(message), nil
}

func (m *Mailbox) Accept(message Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	key := mailboxKey(message.SessionID, message.To)
	if message.Sequence > m.seq[key] {
		m.seq[key] = message.Sequence
	}
	message.Body = append(json.RawMessage(nil), message.Body...)
	m.pending = append(m.pending, message)
}

func (m *Mailbox) prepareLocked(message Message) Message {
	if message.SessionID == "" {
		message.SessionID = m.defaultSession
	}
	key := mailboxKey(message.SessionID, message.To)
	m.seq[key]++
	message.Sequence = m.seq[key]
	if message.ID == "" {
		message.ID = fmt.Sprintf(
			"message-%x-%s-%d",
			sha256.Sum256([]byte(message.SessionID)),
			message.To,
			message.Sequence,
		)
	}
	if message.Kind == "" {
		message.Kind = MessageContext
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	message.Body = append(json.RawMessage(nil), message.Body...)
	return message
}

func (m *Mailbox) Drain(to string) []Message {
	messages := m.Receive(to)
	if err := m.Ack(messages); err != nil {
		// Ack failure must not hide already received messages. Delivery can
		// retry from pending state; callers still need the payload now.
		return messages
	}
	return messages
}

func (m *Mailbox) Receive(to string) []Message {
	return m.ReceiveSession("", to)
}

func (m *Mailbox) ReceiveSession(sessionID, to string) []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Message
	for _, message := range m.pending {
		if (sessionID == "" || message.SessionID == sessionID) &&
			(to == "" || message.To == to) {
			message.Body = append(json.RawMessage(nil), message.Body...)
			out = append(out, message)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

func (m *Mailbox) Pending(to string) []Message { return m.Receive(to) }

func (m *Mailbox) PendingSession(sessionID, to string) []Message {
	return m.ReceiveSession(sessionID, to)
}

func (m *Mailbox) Ack(messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	delivered := time.Now().UTC()
	for index := range messages {
		messages[index].DeliveredAt = &delivered
		if m.deliver != nil {
			if err := m.deliver(messages[index]); err != nil {
				return err
			}
		}
	}
	acknowledged := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		acknowledged[message.ID] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.pending[:0]
	for _, message := range m.pending {
		if _, ok := acknowledged[message.ID]; !ok {
			kept = append(kept, message)
		}
	}
	m.pending = kept
	return nil
}

func (m *Mailbox) Restore(messages []Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := make(map[string]struct{}, len(m.pending))
	for _, message := range m.pending {
		existing[message.ID] = struct{}{}
	}
	for _, message := range messages {
		key := mailboxKey(message.SessionID, message.To)
		if message.Sequence > m.seq[key] {
			m.seq[key] = message.Sequence
		}
		if message.DeliveredAt == nil {
			if _, ok := existing[message.ID]; ok {
				continue
			}
			message.Body = append(json.RawMessage(nil), message.Body...)
			m.pending = append(m.pending, message)
			existing[message.ID] = struct{}{}
		}
	}
}

func mailboxKey(sessionID, to string) string { return sessionID + "\x00" + to }
