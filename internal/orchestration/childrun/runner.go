// Package childrun runs spawned agents as first-class runtime turns on their
// own threads. It owns the stateful child turn lifecycle — residency,
// wall-time leases and settlement — and reaches the session Runtime only
// through the narrow Host, Threads and ToolPlanes interfaces that wire binds.
// Child budgets belong to subagent.Manager, which admits and reserves a turn
// before StartTurn and charges its receipt when the Runner settles it.
package childrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Host is the part of the session Runtime a child turn goes through. A child
// turn is an ordinary Submit, so every tool call, approval and receipt it
// produces is an ordinary event that the eventlog, replay and SSE already carry.
type Host interface {
	ObserveEvents(observer func(protocol.Event)) func()
	SessionProfilesAvailable() bool
	RestoreSessionProfile(
		ctx context.Context, sessionID string, threadID protocol.ThreadID,
	) (protocol.SessionProfileSnapshot, error)
	Submit(ctx context.Context, operation protocol.Operation) error
}

// Threads is the thread registry a child's engine is registered in.
type Threads interface {
	ChildSpecFor(threadID protocol.ThreadID) (app.ChildSpec, bool)
	RegisterChild(threadID protocol.ThreadID, spec app.ChildSpec) error
	Release(threadID protocol.ThreadID)
	EstimateFirstWindow(threadID protocol.ThreadID, prompt string) (uint64, uint64, error)
}

// ToolPlanes owns the isolated tool planes of writing children. A closed
// child's plane is released here because this is where its lifetime ends.
type ToolPlanes interface {
	Release(root string)
}

// Options configures a Runner before the session Runtime exists.
type Options struct {
	Limits config.Subagent
	// Workspace is the host workspace children read and lease worktrees from.
	Workspace string
	Tools     ToolPlanes
}

// Runner is constructed before the Runtime exists — the agent tool has to be
// registered while the tool registry is still being built — and bound
// afterwards.
type Runner struct {
	limits config.Subagent
	root   string
	tools  ToolPlanes

	mu               sync.Mutex
	host             Host
	threads          Threads
	manager          *subagent.AgentControl
	turns            map[protocol.ThreadID]*childTurn
	bound            bool
	closing          bool
	settlementErrors map[protocol.TurnID]error

	removeObserver func()
	stop           chan struct{}
	stopOnce       sync.Once
	settlers       sync.WaitGroup
}

// childTurn accumulates what a child turn observed until its terminal event
// says how to settle it.
type childTurn struct {
	agentID        string
	turnID         protocol.TurnID
	startOperation protocol.OperationID
	started        bool
	settling       bool
	receipt        *protocol.ExecutionReceiptData
	verify         *protocol.TurnVerificationData
	text           string
	notes          []string
	failure        subagent.SettlementFailure
	deadline       context.CancelFunc
	leaseRenewal   chan struct{}
	timedOut       bool
	startedAt      time.Time
	releasePending bool
	startedSignal  chan struct{}
	terminalSignal chan struct{}
}

func New(options Options) *Runner {
	return &Runner{
		limits: options.Limits, root: options.Workspace, tools: options.Tools,
		turns:            make(map[protocol.ThreadID]*childTurn),
		settlementErrors: make(map[protocol.TurnID]error),
		stop:             make(chan struct{}),
	}
}

// Bind attaches the pieces that only exist once the Runtime is constructed
// and re-arms observation of children that were running before a restart.
func (c *Runner) Bind(
	host Host, threads Threads, manager *subagent.AgentControl,
) error {
	if host == nil || threads == nil || manager == nil {
		return errors.New("child runtime dependencies are incomplete")
	}
	c.mu.Lock()
	c.host = host
	c.threads = threads
	c.manager = manager
	c.bound = true
	c.mu.Unlock()
	c.mu.Lock()
	c.removeObserver = host.ObserveEvents(c.Observe)
	c.mu.Unlock()
	var recovered []struct {
		threadID protocol.ThreadID
		turnID   protocol.TurnID
	}
	for _, agent := range manager.List(subagent.ListFilter{}) {
		switch agent.Status {
		case subagent.StatusStarting, subagent.StatusRunning,
			subagent.StatusWaiting:
		default:
			continue
		}
		evicted, err := manager.ActivateResident(agent.ID)
		if err != nil {
			return fmt.Errorf("restore child residency for %s: %w", agent.ID, err)
		}
		for _, unloaded := range evicted {
			c.unloadThread(unloaded.ID)
		}
		threadID := protocol.ThreadID(agent.ThreadID)
		if _, registered := threads.ChildSpecFor(threadID); !registered {
			spec, err := c.Spec(agent)
			if err != nil {
				return fmt.Errorf("restore child authority for %s: %w", agent.ID, err)
			}
			if err := threads.RegisterChild(threadID, spec); err != nil {
				return fmt.Errorf("restore child thread %s: %w", threadID, err)
			}
		}
		if agent.TurnID == "" ||
			(agent.Status != subagent.StatusStarting &&
				agent.Status != subagent.StatusRunning &&
				agent.Status != subagent.StatusWaiting) {
			continue
		}
		turnID := protocol.TurnID(agent.TurnID)
		recoveredTurn := &childTurn{
			agentID: agent.ID, turnID: turnID, startedAt: time.Now(),
			leaseRenewal:   make(chan struct{}, 1),
			startedSignal:  make(chan struct{}),
			terminalSignal: make(chan struct{}),
		}
		c.mu.Lock()
		if _, tracked := c.turns[threadID]; !tracked {
			c.turns[threadID] = recoveredTurn
			recovered = append(recovered, struct {
				threadID protocol.ThreadID
				turnID   protocol.TurnID
			}{threadID: threadID, turnID: turnID})
		}
		c.mu.Unlock()
	}
	for _, turn := range recovered {
		c.armDeadline(turn.threadID, turn.turnID)
	}
	return nil
}

func (c *Runner) Close() {
	c.mu.Lock()
	c.closing = true
	removeObserver := c.removeObserver
	c.removeObserver = nil
	for _, turn := range c.turns {
		if turn.deadline != nil {
			turn.deadline()
		}
	}
	c.mu.Unlock()
	if removeObserver != nil {
		removeObserver()
	}
	c.stopOnce.Do(func() { close(c.stop) })
	c.settlers.Wait()
}

// StartTurn submits a real turn for the child agent and returns as soon as the
// runtime accepted it. Blocking until the child finishes would make wait_agent
// pointless and would deadlock the parent turn that called the agent tool.
func (c *Runner) StartTurn(ctx context.Context, agentID, prompt string) (string, error) {
	c.mu.Lock()
	// Manager publishes the terminal result before the runtime finishes its
	// settlement bookkeeping. A follow-up must not replace that tracked turn.
	if previous := c.turns[protocol.ThreadID(subagent.ThreadIDFor(agentID))]; previous != nil {
		terminal, settling := previous.terminalSignal, previous.settling
		c.mu.Unlock()
		if !settling {
			return "", errors.New("child turn is still active")
		}
		select {
		case <-terminal:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.stop:
			return "", errors.New("child runtime is closed")
		}
		c.mu.Lock()
	}
	host, threads, manager, bound :=
		c.host, c.threads, c.manager, c.bound
	var settlementErrors []error
	for _, err := range c.settlementErrors {
		settlementErrors = append(settlementErrors, err)
	}
	c.mu.Unlock()
	if runtimeErr := errors.Join(settlementErrors...); runtimeErr != nil {
		return "", protocol.NewProblem(
			protocol.CodeUnavailable,
			"child settlement recovery is pending",
			true,
			runtimeErr,
		)
	}
	if !bound {
		return "", protocol.NewProblem(
			protocol.CodeUnavailable, "child agent runtime is not bound to a session", false, nil,
		)
	}
	agent, ok := manager.Agent(agentID)
	if !ok {
		return "", fmt.Errorf("agent %s is unavailable", agentID)
	}
	spec, err := c.Spec(agent)
	if err != nil {
		return "", err
	}
	turnID, err := protocol.NewTurnID()
	if err != nil {
		return "", err
	}
	itemID, err := protocol.NewItemID()
	if err != nil {
		return "", err
	}
	newResident := !agent.Resident
	evicted, err := manager.ActivateResident(agentID)
	if err != nil {
		return "", err
	}
	for _, unloaded := range evicted {
		c.unloadThread(unloaded.ID)
	}
	rollbackResident := func() {
		if newResident {
			c.unloadThread(agentID)
		}
	}
	threadID := protocol.ThreadID(subagent.ThreadIDFor(agentID))
	if _, registered := threads.ChildSpecFor(threadID); !registered {
		if err := threads.RegisterChild(threadID, spec); err != nil {
			rollbackResident()
			return "", err
		}
	}
	if host.SessionProfilesAvailable() && agent.SessionID != "" {
		if _, err := host.RestoreSessionProfile(
			ctx, agent.SessionID, threadID,
		); err != nil {
			rollbackResident()
			return "", fmt.Errorf("restore child session profile: %w", err)
		}
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: itemID, Prompt: prompt,
		Intent: turnIntent(agent.Role, spec.ReadOnly),
	})
	if err != nil {
		rollbackResident()
		return "", err
	}
	c.mu.Lock()
	c.turns[threadID] = &childTurn{
		agentID: agentID, turnID: turnID, startOperation: operation.ID,
		startedAt:      time.Now(),
		leaseRenewal:   make(chan struct{}, 1),
		startedSignal:  make(chan struct{}),
		terminalSignal: make(chan struct{}),
	}
	c.mu.Unlock()

	if err := host.Submit(ctx, operation); err != nil {
		c.mu.Lock()
		delete(c.turns, threadID)
		c.mu.Unlock()
		rollbackResident()
		return "", err
	}
	c.armDeadline(threadID, turnID)
	return string(turnID), nil
}

func turnIntent(role subagent.Role, readOnly bool) protocol.TurnIntent {
	switch role {
	case subagent.RolePlan:
		return protocol.TurnIntentPlan
	case subagent.RoleImplementer, subagent.RoleGeneral:
		if !readOnly {
			return protocol.TurnIntentWorkspaceChange
		}
		return protocol.TurnIntentAnswer
	default:
		return protocol.TurnIntentAnswer
	}
}

// CancelTurn interrupts a child turn through the same cancel operation a host
// would use, so a child's cancellation is as auditable as any other.
func (c *Runner) CancelTurn(ctx context.Context, agentID, turnID string) error {
	c.mu.Lock()
	host, bound := c.host, c.bound
	active := c.turns[protocol.ThreadID(subagent.ThreadIDFor(agentID))]
	var started, terminal <-chan struct{}
	if active != nil && active.turnID == protocol.TurnID(turnID) {
		terminal = active.terminalSignal
		if !active.started {
			started = active.startedSignal
		}
		if active.settling {
			c.mu.Unlock()
			return nil
		}
	}
	c.mu.Unlock()
	if !bound {
		return nil
	}
	// Accepted starts are asynchronous. Canceling before turn.started could be
	// rejected as "not active", leaving Close waiting for an uncanceled turn.
	if started != nil {
		select {
		case <-started:
		case <-terminal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stop:
			return errors.New("child runtime is closed")
		}
	}
	itemID, err := protocol.NewItemID()
	if err != nil {
		return err
	}
	operation, err := protocol.NewOperation(&protocol.CancelTurnPayload{
		ThreadID: protocol.ThreadID(subagent.ThreadIDFor(agentID)),
		TurnID:   protocol.TurnID(turnID),
		ItemID:   itemID,
		Reason:   protocol.CancelReasonHostInterrupted,
	})
	if err != nil {
		return err
	}
	return host.Submit(ctx, operation)
}

// Release drops a closed child's thread engine so its history and guard are
// not retained for the rest of the process.
func (c *Runner) Release(agentID string) {
	threadID := protocol.ThreadID(subagent.ThreadIDFor(agentID))
	c.mu.Lock()
	active := c.turns[threadID]
	if active == nil {
		c.mu.Unlock()
		c.releaseThread(threadID)
		return
	}
	active.releasePending = true
	started := active.started && !active.settling
	settling := active.settling
	turnID := active.turnID
	terminal := active.terminalSignal
	c.mu.Unlock()
	if !started && !settling {
		return
	}
	if started {
		c.cancelReleased(agentID, turnID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case <-terminal:
	case <-ctx.Done():
	}
}

func (c *Runner) cancelReleased(
	agentID string,
	turnID protocol.TurnID,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.CancelTurn(ctx, agentID, string(turnID))
	cancel()
}

func (c *Runner) releaseThread(threadID protocol.ThreadID) {
	c.mu.Lock()
	threads := c.threads
	if turn := c.turns[threadID]; turn != nil {
		if turn.deadline != nil {
			turn.deadline()
		}
	}
	delete(c.turns, threadID)
	c.mu.Unlock()
	if threads == nil {
		return
	}
	// The spec has to be read before the thread is released, because releasing it
	// is what forgets which isolated root this child was using.
	if spec, ok := threads.ChildSpecFor(threadID); ok &&
		!spec.ReadOnly && !spec.Serialized && c.tools != nil {
		c.tools.Release(spec.Workspace)
	}
	threads.Release(threadID)
	if c.manager != nil {
		if agent, ok := c.manager.AgentByThread(string(threadID)); ok {
			c.manager.DeactivateResident(agent.ID)
		}
	}
}

func (c *Runner) unloadThread(agentID string) {
	threadID := protocol.ThreadID(subagent.ThreadIDFor(agentID))
	c.mu.Lock()
	active := c.turns[threadID] != nil
	c.mu.Unlock()
	if active {
		return
	}
	c.releaseThread(threadID)
	if c.manager != nil {
		c.manager.DeactivateResident(agentID)
	}
}

// Spec resolves where an agent runs and what it may do there. It fails closed:
// a child that needs to write but has nowhere isolated to write is rejected
// rather than pointed at the parent workspace.
func (c *Runner) Spec(agent subagent.Agent) (app.ChildSpec, error) {
	role, err := c.manager.RoleSpec(agent.Role)
	if err != nil {
		return app.ChildSpec{}, err
	}
	var parentThreadID protocol.ThreadID
	if agent.Context != nil {
		parentThreadID = protocol.ThreadID(agent.Context.SourceThread)
	}
	if parentThreadID == "" && agent.Parent != "" {
		parentThreadID = protocol.ThreadID(subagent.ThreadIDFor(agent.Parent))
	}
	spec := app.ChildSpec{
		AgentID: agent.ID, ParentThreadID: parentThreadID,
		AgentPath: agent.Path, ParentPath: agent.ParentPath,
		Role: string(agent.Role), Stance: string(agent.Stance),
		Workspace: c.root, HostWorkspace: agent.Workspace, SessionID: agent.SessionID,
		ReadOnly:     true,
		AllowedTools: append([]string(nil), role.AllowedTools...),
		CanDelegate:  role.CanDelegate,
		MaxSteps:     agent.Budget.MaxSteps, MaxTokens: agent.Budget.MaxTokens,
		MaxCostUSD: agent.Budget.MaxCostUSD,
	}
	if spec.MaxSteps == 0 {
		spec.MaxSteps = c.limits.MaxSteps
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = c.limits.MaxTokens
	}
	if spec.MaxCostUSD == 0 {
		spec.MaxCostUSD = c.limits.MaxCostUSD
	}
	if spec.HostWorkspace == "" {
		spec.HostWorkspace = c.root
	}
	if c.limits.Workspace == config.SubagentWorkspaceSerialized {
		if !agent.Serialized || strings.TrimSpace(agent.Worktree) != c.root {
			return app.ChildSpec{}, protocol.NewProblem(
				protocol.CodeUnavailable,
				"serialized child does not own the configured host workspace lease",
				false, nil,
			)
		}
		spec.Serialized = true
		spec.ReadOnly = agent.Stance == subagent.StanceReadOnly
		return spec, nil
	}
	if c.limits.Workspace == config.SubagentWorkspaceReadOnly ||
		agent.Stance == subagent.StanceReadOnly {
		// Read-only children share the host workspace and get no journal: they
		// change nothing, so there is nothing to roll back.
		if strings.TrimSpace(agent.ExecutionRoot) != "" {
			spec.Workspace = agent.ExecutionRoot
		}
		return spec, nil
	}
	if !agent.Isolated || strings.TrimSpace(agent.Worktree) == "" {
		return app.ChildSpec{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf(
				"child agents with stance %q need an isolated worktree, which this workspace "+
					"could not provide; spawn an explore or review agent, or set "+
					"execution.subagent.workspace = %q to run this child read-only",
				agent.Stance, config.SubagentWorkspaceReadOnly,
			),
			false, nil,
		)
	}
	spec.Workspace, spec.ReadOnly = agent.Worktree, false
	return spec, nil
}

func (c *Runner) armDeadline(threadID protocol.ThreadID, turnID protocol.TurnID) {
	wallTime := c.limits.WallTime
	if wallTime <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	turn := c.turns[threadID]
	if turn == nil || turn.turnID != turnID {
		c.mu.Unlock()
		cancel()
		return
	}
	turn.deadline = cancel
	c.mu.Unlock()

	go func() {
		timer := time.NewTimer(wallTime)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-turn.leaseRenewal:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(wallTime)
			case <-timer.C:
				goto expired
			}
		}
	expired:
		c.mu.Lock()
		current := c.turns[threadID]
		if current == nil || current.turnID != turnID {
			c.mu.Unlock()
			return
		}
		current.timedOut = true
		agentID := current.agentID
		c.mu.Unlock()
		// An idle lease expires through the normal cancel path. The terminal
		// event preserves it as an interrupted child that can be taken over.
		_ = c.CancelTurn(context.Background(), agentID, string(turnID))
	}()
}

// Observe is the event observer Bind registers with the Host. Events for
// untracked threads, other turns, or turns already settling are ignored, so a
// replayed terminal event never settles a child twice.
func (c *Runner) Observe(event protocol.Event) {
	if event.ThreadID == "" {
		return
	}
	c.mu.Lock()
	turn := c.turns[event.ThreadID]
	if turn == nil || turn.settling || (event.TurnID != "" && event.TurnID != turn.turnID) {
		c.mu.Unlock()
		return
	}
	settle := false
	status := subagent.StatusCompleted
	var waitRequest, resumeRequest string
	cancelRelease := false
	switch data := event.Data.(type) {
	case *protocol.TurnStartedData:
		if !turn.started && turn.startedSignal != nil {
			close(turn.startedSignal)
		}
		turn.started = true
		cancelRelease = turn.releasePending
	case *protocol.ExecutionReceiptData:
		copied := *data
		turn.receipt = &copied
	case *protocol.TurnVerificationData:
		copied := *data
		turn.verify = &copied
	case *protocol.ApprovalRequiredData:
		waitRequest = data.RequestID
	case *protocol.ApprovalResolvedData:
		resumeRequest = data.RequestID
	case *protocol.TurnCompletedData:
		if text := strings.TrimSpace(data.Text); text != "" {
			turn.text = text
		}
		settle, status = true, subagent.StatusCompleted
	case *protocol.TurnFailedData:
		turn.failure = subagent.SettlementFailure{
			Code: data.Code, Message: data.Message,
			Fault: protocol.CloneFaultMetadata(data.Fault),
		}
		turn.notes = append(turn.notes, fmt.Sprintf("%s: %s", data.Code, data.Message))
		settle, status = true, subagent.StatusErrored
	case *protocol.TurnCanceledData:
		settle, status = true, subagent.StatusInterrupted
	case *protocol.OperationRejectedData:
		turn.notes = append(turn.notes, fmt.Sprintf(
			"operation rejected: %s",
			data.Message,
		))
		if event.OperationID == turn.startOperation {
			turn.failure = subagent.SettlementFailure{
				Code: data.Code, Message: data.Message,
				Fault: protocol.CloneFaultMetadata(data.Fault),
			}
			settle, status = true, subagent.StatusErrored
		}
	}
	if !settle {
		select {
		case turn.leaseRenewal <- struct{}{}:
		default:
		}
		manager := c.manager
		agentID := turn.agentID
		turnID := turn.turnID
		c.mu.Unlock()
		var transitionErr error
		if manager != nil && waitRequest != "" {
			transitionErr = manager.AwaitApproval(agentID, waitRequest)
		}
		if manager != nil && resumeRequest != "" {
			transitionErr = manager.ResumeApproval(agentID, resumeRequest)
		}
		if transitionErr != nil {
			c.mu.Lock()
			if current := c.turns[event.ThreadID]; current != nil {
				current.notes = append(current.notes, transitionErr.Error())
			}
			c.mu.Unlock()
		}
		if cancelRelease {
			go c.cancelReleased(agentID, turnID)
		}
		return
	}
	if turn.deadline != nil {
		turn.deadline()
		turn.deadline = nil
	}
	result := turn.result(event.ThreadID, status)
	manager := c.manager
	turn.settling = true
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.settlers.Add(1)
	c.mu.Unlock()

	go c.settleChild(
		event.ThreadID, turn, result, manager,
	)
}

func (c *Runner) settleChild(
	threadID protocol.ThreadID,
	turn *childTurn,
	result subagent.Result,
	manager *subagent.AgentControl,
) {
	defer c.settlers.Done()
	if err := c.settleChildAttempt(turn, result, manager); err == nil {
		c.completeChildSettlement(
			threadID,
			turn,
			manager,
		)
		return
	} else {
		c.recordSettlementError(turn, err)
	}
	c.retryChildSettlement(
		threadID, turn, result, manager,
	)
}

func (c *Runner) settleChildAttempt(
	_ *childTurn,
	result subagent.Result,
	manager *subagent.AgentControl,
) error {
	if manager != nil {
		return manager.Settle(result)
	}
	return nil
}

func (c *Runner) completeChildSettlement(
	threadID protocol.ThreadID,
	turn *childTurn,
	manager *subagent.AgentControl,
) {
	if manager != nil {
		manager.TouchResident(turn.agentID)
	}
	c.mu.Lock()
	delete(c.settlementErrors, turn.turnID)
	releasePending := turn.releasePending
	delete(c.turns, threadID)
	c.mu.Unlock()
	if releasePending {
		c.releaseThread(threadID)
	}
	if turn.terminalSignal != nil {
		close(turn.terminalSignal)
	}
}

func (c *Runner) recordSettlementError(
	turn *childTurn,
	err error,
) {
	c.mu.Lock()
	c.settlementErrors[turn.turnID] = fmt.Errorf(
		"settle child %s turn %s: %w",
		turn.agentID,
		turn.turnID,
		err,
	)
	c.mu.Unlock()
}

func (c *Runner) retryChildSettlement(
	threadID protocol.ThreadID,
	turn *childTurn,
	result subagent.Result,
	manager *subagent.AgentControl,
) {
	delay := 25 * time.Millisecond
	for {
		timer := time.NewTimer(delay)
		select {
		case <-c.stop:
			timer.Stop()
			if turn.terminalSignal != nil {
				close(turn.terminalSignal)
			}
			return
		case <-timer.C:
		}
		if err := c.settleChildAttempt(turn, result, manager); err == nil {
			c.completeChildSettlement(
				threadID,
				turn,
				manager,
			)
			return
		} else {
			c.recordSettlementError(turn, err)
		}
		delay = min(delay*2, time.Second)
	}
}

func (c *Runner) EstimateTurn(
	ctx context.Context,
	agentID, prompt string,
) (subagent.TurnEstimate, error) {
	c.mu.Lock()
	threads, manager, bound := c.threads, c.manager, c.bound
	c.mu.Unlock()
	if !bound || threads == nil || manager == nil {
		return subagent.TurnEstimate{}, protocol.NewProblem(
			protocol.CodeUnavailable, "child agent runtime is not bound to a session", false, nil,
		)
	}
	agent, ok := manager.Agent(agentID)
	if !ok {
		return subagent.TurnEstimate{}, fmt.Errorf("agent %s is unavailable", agentID)
	}
	spec, err := c.Spec(agent)
	if err != nil {
		return subagent.TurnEstimate{}, err
	}
	threadID := protocol.ThreadID(subagent.ThreadIDFor(agentID))
	_, registered := threads.ChildSpecFor(threadID)
	if !registered {
		if err := threads.RegisterChild(threadID, spec); err != nil {
			return subagent.TurnEstimate{}, err
		}
	}
	projected, limit, err := threads.EstimateFirstWindow(threadID, prompt)
	if !registered {
		c.releaseThread(threadID)
	}
	if err != nil {
		return subagent.TurnEstimate{}, err
	}
	if agent.Budget.MaxTokens > 0 {
		limit = agent.Budget.MaxTokens
	}
	return subagent.TurnEstimate{
		ProjectedTokens: projected, LimitTokens: limit,
	}, nil
}

func (t *childTurn) result(threadID protocol.ThreadID, status subagent.Status) subagent.Result {
	result := subagent.Result{
		AgentID: t.agentID, ThreadID: string(threadID), TurnID: string(t.turnID),
		Status: status, Summary: t.text,
	}
	if t.timedOut {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf(
			"child execution lease expired after %s without runtime progress",
			time.Since(t.startedAt).Round(time.Second),
		))
	}
	result.Unresolved = append(result.Unresolved, t.notes...)
	if receipt := t.receipt; receipt != nil {
		result.Evidence = receipt.Evidence
		result.Diff = receipt.Changes
		result.Verification = receipt.Verification
		result.Unresolved = append(result.Unresolved, receipt.UnresolvedIssues...)
		result.PermissionDigests = append(
			[]string(nil),
			receipt.PermissionDigests...,
		)
		result.Usage = subagent.ResultUsage{
			InputTokens: receipt.InputTokens, OutputTokens: receipt.OutputTokens,
			ReasoningTokens: receipt.ReasoningTokens, CachedTokens: receipt.CachedTokens,
			CostMicrounits: receipt.CostMicrounits, CostKnown: receipt.CostKnown,
		}
	} else {
		// No receipt means the turn never reached its own accounting: say so
		// instead of reporting an all-zero, all-passed result.
		result.Verification = protocol.ReceiptVerification{
			Diagnostics: protocol.ReceiptNotEvaluated,
			Tests:       protocol.ReceiptNotEvaluated,
			Verify:      protocol.ReceiptNotEvaluated,
		}
	}
	if t.verify != nil && t.verify.Status != "" {
		result.Verification.Verify = t.verify.Status
	}
	result.ReasonCode, result.Summary, result.Retryable = subagent.ClassifySettlement(
		status, t.failure, result.Unresolved, result.Summary,
	)
	if result.Summary == "" {
		result.Summary = t.text
	}
	if result.Retryable || status == subagent.StatusInterrupted {
		result.SuggestedAction = subagent.SuggestedAction(result.ReasonCode)
	}
	if status == subagent.StatusFailed && t.failure.Fault != nil {
		if action := strings.TrimSpace(t.failure.Fault.RecoveryAction); action != "" {
			result.SuggestedAction = action
		}
	}
	return result
}
