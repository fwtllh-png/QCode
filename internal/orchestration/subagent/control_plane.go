package subagent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
)

// DelegationMode controls whether and why a model may create child agents.
type DelegationMode string

const (
	DelegationDisabled DelegationMode = "disabled"
	DelegationExplicit DelegationMode = "explicit"
	DelegationAdaptive DelegationMode = "adaptive"
)

// DelegationTrigger records the authority that caused a spawn proposal.
type DelegationTrigger string

const (
	TriggerUser      DelegationTrigger = "user"
	TriggerDeveloper DelegationTrigger = "developer"
	TriggerSkill     DelegationTrigger = "skill"
	TriggerSystem    DelegationTrigger = "system"
	TriggerAdaptive  DelegationTrigger = "adaptive"
)

// DelegationIntent is the structured contract for creating one child.
type DelegationIntent struct {
	SessionID      string
	TaskName       string
	Role           Role
	Objective      string
	ExpectedOutput string
	OwnedPaths     []string
	ParentID       string
	Trigger        DelegationTrigger
	Budget         AgentBudget
	TraceParent    string
	TraceState     string
}

// DelegationPolicy validates the provenance of a spawn proposal. It is an
// orchestration policy, not a replacement for Tool Guard or security policy.
type DelegationPolicy struct {
	mode DelegationMode
}

func NewDelegationPolicy(mode DelegationMode) (DelegationPolicy, error) {
	switch mode {
	case DelegationDisabled, DelegationExplicit, DelegationAdaptive:
		return DelegationPolicy{mode: mode}, nil
	default:
		return DelegationPolicy{}, fmt.Errorf("unsupported delegation mode %q", mode)
	}
}

func (p DelegationPolicy) Mode() DelegationMode { return p.mode }

func (p DelegationPolicy) ModelVisible() bool {
	return p.mode != DelegationDisabled
}

func (p DelegationPolicy) Admit(intent DelegationIntent) error {
	if err := validateIntent(intent); err != nil {
		return err
	}
	switch p.mode {
	case DelegationDisabled:
		if intent.Trigger == TriggerSystem {
			return nil
		}
		return errors.New("subagent delegation is disabled")
	case DelegationExplicit:
		switch intent.Trigger {
		case TriggerUser, TriggerDeveloper, TriggerSkill, TriggerSystem:
			return nil
		default:
			return fmt.Errorf(
				"delegation mode explicit rejects trigger %q; explicit user, developer, skill, or system authority is required",
				intent.Trigger,
			)
		}
	case DelegationAdaptive:
		switch intent.Trigger {
		case TriggerUser, TriggerDeveloper, TriggerSkill, TriggerSystem, TriggerAdaptive:
			return nil
		default:
			return fmt.Errorf("unsupported delegation trigger %q", intent.Trigger)
		}
	default:
		return fmt.Errorf("unsupported delegation mode %q", p.mode)
	}
}

// Instructions returns the stable developer-facing delegation contract.
func (p DelegationPolicy) Instructions() string {
	const contextContract = " Context is runtime-owned: omit context_mode to use a bounded task_capsule; " +
		"use last_n_turns only when recent history is material, and use full only with explicit authority. " +
		"Never copy parent transcripts or secrets into tool arguments."
	switch p.mode {
	case DelegationDisabled:
		return ""
	case DelegationExplicit:
		return "Multi-agent delegation is explicit-only. Use spawn_agent only when the user, " +
			"developer instructions, an active skill, or an internal system task explicitly authorizes delegation. " +
			"Do not treat task complexity alone as authorization. Prefer existing agents via followup_task, " +
			"run independent children concurrently, use wait_agent for synchronization, and close agents when done." +
			contextContract
	case DelegationAdaptive:
		return "Multi-agent delegation is adaptive. Investigate in the parent first. Spawn a child only when the " +
			"parallel benefit is explicit and the work is independently scoped. Do not open several review agents " +
			"just because a task spans modules. Prefer followup_task on an existing resident agent, use wait_agent " +
			"for synchronization, and close agents when done." + contextContract
	default:
		return ""
	}
}

func validateIntent(intent DelegationIntent) error {
	if strings.TrimSpace(intent.TaskName) == "" {
		return errors.New("task_name is required")
	}
	if strings.TrimSpace(intent.Objective) == "" {
		return errors.New("objective is required")
	}
	if strings.TrimSpace(intent.ExpectedOutput) == "" {
		return errors.New("expected_output is required")
	}
	if intent.Trigger == "" {
		return errors.New("delegation trigger is required")
	}
	if intent.Budget.MaxSteps < 0 || intent.Budget.MaxCostUSD < 0 {
		return errors.New("child budget limits must be non-negative")
	}
	for _, path := range intent.OwnedPaths {
		clean := filepath.Clean(strings.TrimSpace(path))
		if clean == "" || clean == "." || filepath.IsAbs(clean) ||
			clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("owned path %q must be workspace-relative", path)
		}
	}
	return nil
}

// RoleSpec defines a real child execution role rather than a profile label.
type RoleSpec struct {
	Role          Role
	Profile       string
	Stance        Stance
	Instructions  string
	AllowedTools  []string
	CanDelegate   bool
	FullContext   bool
	DefaultBudget Budget
}

// RoleCatalog resolves immutable built-in role contracts.
type RoleCatalog struct {
	specs map[Role]RoleSpec
}

func DefaultRoleCatalog() RoleCatalog {
	return NewRoleCatalog([]RoleSpec{
		{
			Role: RoleGeneral, Profile: "default", Stance: StanceWrite,
			Instructions: "Complete the assigned objective within its declared scope and return evidence.",
			CanDelegate:  true,
		},
		{
			Role: RoleExplore, Profile: "explore", Stance: StanceReadOnly,
			Instructions: "Investigate the narrow question, cite key files and evidence, and do not modify the workspace.",
			AllowedTools: []string{"read", "search"}, CanDelegate: true,
		},
		{
			Role: RolePlan, Profile: "plan", Stance: StanceMinimalWrite,
			Instructions: "Produce a bounded implementation plan, risks, ownership boundaries, and validation criteria.",
			AllowedTools: []string{"read", "search"}, CanDelegate: true,
		},
		{
			Role: RoleReview, Profile: "review", Stance: StanceReadOnly,
			Instructions: "Review independently for correctness and regressions; lead with evidence-backed findings.",
			AllowedTools: []string{"read", "search", "process.read_only"}, CanDelegate: false,
		},
		{
			Role: RoleImplementer, Profile: "implement", Stance: StanceWrite,
			Instructions: "Implement only the assigned objective and owned paths, then verify the resulting behavior.",
			CanDelegate:  true,
		},
		{
			Role: RoleVerifier, Profile: "verify", Stance: StanceTestFocused,
			Instructions: "Verify the supplied behavior independently and return commands, outcomes, and residual risk.",
			AllowedTools: []string{"read", "search", "verify"}, CanDelegate: false,
		},
		{
			Role: RoleAwaiter, Profile: "await", Stance: StanceReadOnly,
			Instructions: "Wait for the assigned long-running work and report only structured progress or terminal state.",
			AllowedTools: []string{"read"}, CanDelegate: false,
		},
		{
			Role: RoleCustom, Profile: "custom", Stance: StanceCustom,
			Instructions: "Follow the explicit custom role instructions without expanding authority or scope.",
			CanDelegate:  false,
		},
	})
}

func (c *AgentControl) TakeoverBackground(
	ctx context.Context,
	agent Agent,
	objective string,
) (string, error) {
	fork, err := c.ForkContext(ctx, ContextRequest{
		Mode: ContextTaskCapsule, Agent: agent, Objective: objective,
		Trigger: TriggerSystem,
	})
	if err != nil {
		return "", err
	}
	return c.Takeover(ctx, agent.ID, fork.Prompt)
}

func NewRoleCatalog(specs []RoleSpec) RoleCatalog {
	catalog := RoleCatalog{specs: make(map[Role]RoleSpec, len(specs))}
	for _, spec := range specs {
		cloned := spec
		cloned.AllowedTools = append([]string(nil), spec.AllowedTools...)
		catalog.specs[spec.Role] = cloned
	}
	return catalog
}

func (c RoleCatalog) Resolve(role Role) (RoleSpec, error) {
	spec, ok := c.specs[role]
	if !ok {
		return RoleSpec{}, fmt.Errorf("unsupported role %q", role)
	}
	spec.AllowedTools = append([]string(nil), spec.AllowedTools...)
	return spec, nil
}

func (c RoleCatalog) Roles() []Role {
	roles := make([]Role, 0, len(c.specs))
	for role := range c.specs {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return roles
}

// AgentControl is the single lifecycle entry used by tools, workers, and hosts.
// Manager remains the synchronized state owner behind this boundary.
type AgentControl struct {
	manager     *Manager
	roles       RoleCatalog
	policy      DelegationPolicy
	forker      *ContextForker
	providerHot func() bool
}

func NewAgentControl(
	manager *Manager,
	roles RoleCatalog,
	policy DelegationPolicy,
) (*AgentControl, error) {
	if manager == nil {
		return nil, errors.New("agent control requires a manager")
	}
	if len(roles.specs) == 0 {
		roles = DefaultRoleCatalog()
	}
	return &AgentControl{
		manager: manager, roles: roles, policy: policy,
		forker: NewContextForker(DefaultContextPolicy()),
	}, nil
}

// OpenControl constructs the synchronized state owner and its only lifecycle
// facade at the orchestration boundary.
func OpenControl(options Options, mode DelegationMode) (*AgentControl, error) {
	manager, err := Open(options)
	if err != nil {
		return nil, err
	}
	policy, err := NewDelegationPolicy(mode)
	if err != nil {
		return nil, err
	}
	return NewAgentControl(manager, options.Roles, policy)
}

func (c *AgentControl) Policy() DelegationPolicy { return c.policy }

func (c *AgentControl) Roles() RoleCatalog { return c.roles }

func (c *AgentControl) BindContextSource(source ContextSource) {
	if c != nil && c.forker != nil {
		c.forker.BindSource(source)
	}
}

func (c *AgentControl) BindProviderGate(hot func() bool) {
	if c == nil {
		return
	}
	c.providerHot = hot
}

func (c *AgentControl) ForkContext(
	ctx context.Context,
	request ContextRequest,
) (ContextFork, error) {
	if c == nil || c.forker == nil {
		return ContextFork{}, errors.New("agent context forker is unavailable")
	}
	if request.Role.Role == "" {
		spec, err := c.roles.Resolve(request.Agent.Role)
		if err != nil {
			return ContextFork{}, err
		}
		request.Role = spec
	}
	request.Role.DefaultBudget = tightenBudget(
		request.Role.DefaultBudget,
		c.manager.budget,
	)
	fork, err := c.forker.Fork(ctx, request)
	if err != nil {
		return ContextFork{}, err
	}
	if request.Agent.ID != "" {
		if err := c.manager.recordContextReceipt(
			request.Agent.ID, fork.Receipt,
		); err != nil {
			return ContextFork{}, err
		}
	}
	return fork, nil
}

func (c *AgentControl) RoleSpec(role Role) (RoleSpec, error) {
	if c == nil {
		return RoleSpec{}, errors.New("agent control is unavailable")
	}
	return c.roles.Resolve(role)
}

func tightenBudget(role, tree Budget) Budget {
	tightenUint := func(value, ceiling uint64) uint64 {
		if value == 0 || ceiling > 0 && ceiling < value {
			return ceiling
		}
		return value
	}
	tightenInt := func(value, ceiling int) int {
		if value == 0 || ceiling > 0 && ceiling < value {
			return ceiling
		}
		return value
	}
	tightenFloat := func(value, ceiling float64) float64 {
		if value == 0 || ceiling > 0 && ceiling < value {
			return ceiling
		}
		return value
	}
	role.MaxTokens = tightenUint(role.MaxTokens, tree.MaxTokens)
	role.MaxCostUSD = tightenFloat(role.MaxCostUSD, tree.MaxCostUSD)
	role.MaxDepth = tightenInt(role.MaxDepth, tree.MaxDepth)
	role.MaxParallel = tightenInt(role.MaxParallel, tree.MaxParallel)
	return role
}

func (c *AgentControl) SpawnIntent(intent DelegationIntent) (*Agent, error) {
	return c.SpawnIntentContext(context.Background(), intent)
}

func (c *AgentControl) SpawnIntentContext(
	ctx context.Context,
	intent DelegationIntent,
) (*Agent, error) {
	if c == nil {
		return nil, errors.New("agent control is unavailable")
	}
	if err := c.policy.Admit(intent); err != nil {
		return nil, err
	}
	spec, err := c.roles.Resolve(intent.Role)
	if err != nil {
		return nil, err
	}
	if traced, traceErr := tracecontext.Child(ctx); traceErr == nil {
		carrier := make(map[string]string, 2)
		tracecontext.InjectMap(traced, carrier)
		intent.TraceParent = carrier[tracecontext.HeaderTraceParent]
		intent.TraceState = carrier[tracecontext.HeaderTraceState]
	}
	return c.manager.spawn(intent, spec)
}

// SpawnInternal preserves the existing internal/test API while ensuring that
// all production paths still pass through RoleCatalog and DelegationPolicy.
func (c *AgentControl) SpawnInternal(parentID string, role Role, objective string) (*Agent, error) {
	return c.SpawnSystem(
		"internal_task", parentID, role, objective,
		"Return a concise result with supporting evidence.",
	)
}

func (c *AgentControl) SpawnSystem(
	taskName, parentID string,
	role Role,
	objective, expectedOutput string,
) (*Agent, error) {
	return c.SpawnIntent(DelegationIntent{
		TaskName: taskName, Role: role, Objective: objective,
		ExpectedOutput: expectedOutput, ParentID: parentID, Trigger: TriggerSystem,
	})
}

func (c *AgentControl) SpawnBackground(role Role, objective string) (*Agent, error) {
	return c.SpawnBackgroundForSession("", role, objective)
}

func (c *AgentControl) SpawnBackgroundForSession(
	sessionID string,
	role Role,
	objective string,
) (*Agent, error) {
	return c.SpawnBackgroundForSessionContext(
		context.Background(),
		sessionID,
		role,
		objective,
	)
}

func (c *AgentControl) SpawnBackgroundForSessionContext(
	ctx context.Context,
	sessionID string,
	role Role,
	objective string,
) (*Agent, error) {
	return c.SpawnIntentContext(ctx, DelegationIntent{
		SessionID: sessionID, TaskName: "background_task",
		Role: role, Objective: objective,
		ExpectedOutput: "Complete the durable task and return structured evidence.",
		Trigger:        TriggerSystem,
	})
}

// Spawn is the concise internal API used by Runtime tests and callers whose
// system authority is already established.
func (c *AgentControl) Spawn(parentID string, role Role, objective string) (*Agent, error) {
	return c.SpawnInternal(parentID, role, objective)
}

func (c *AgentControl) Agent(id string) (Agent, bool) {
	return c.manager.Agent(id)
}

func (c *AgentControl) AgentByThread(threadID string) (Agent, bool) {
	return c.manager.AgentByThread(threadID)
}

func (c *AgentControl) AgentSession(agentID string) (string, bool) {
	return c.manager.AgentSession(agentID)
}

func (c *AgentControl) ActivateResident(agentID string) ([]Agent, error) {
	return c.manager.ActivateResident(agentID)
}

func (c *AgentControl) DeactivateResident(agentID string) {
	c.manager.DeactivateResident(agentID)
}

func (c *AgentControl) TouchResident(agentID string) {
	c.manager.TouchResident(agentID)
}

func (c *AgentControl) IsDescendant(parentID, agentID string) bool {
	return c.manager.IsDescendant(parentID, agentID)
}

func (c *AgentControl) List(filter ListFilter) []Agent {
	return c.manager.List(filter)
}

func (c *AgentControl) Wait(
	ctx context.Context,
	agentIDs []string,
	timeout time.Duration,
) (WaitResult, error) {
	return c.manager.Wait(ctx, agentIDs, timeout)
}

func (c *AgentControl) WaitSession(
	ctx context.Context,
	sessionID string,
	agentIDs []string,
	timeout time.Duration,
) (WaitResult, error) {
	return c.manager.WaitSession(ctx, sessionID, agentIDs, timeout)
}

func (c *AgentControl) FollowUp(ctx context.Context, agentID, prompt string) (string, error) {
	return c.manager.FollowUp(ctx, agentID, prompt)
}

func (c *AgentControl) Interrupt(ctx context.Context, agentID string) (Status, error) {
	return c.manager.Interrupt(ctx, agentID)
}

func (c *AgentControl) AwaitApproval(agentID, requestID string) error {
	return c.manager.AwaitApproval(agentID, requestID)
}

func (c *AgentControl) ResumeApproval(agentID, requestID string) error {
	return c.manager.ResumeApproval(agentID, requestID)
}

func (c *AgentControl) Close(agentID string) error {
	return c.manager.Close(agentID)
}

func (c *AgentControl) CloseContext(ctx context.Context, agentID string) error {
	return c.manager.CloseContext(ctx, agentID)
}

func (c *AgentControl) Takeover(ctx context.Context, agentID, prompt string) (string, error) {
	return c.manager.Takeover(ctx, agentID, prompt)
}

func (c *AgentControl) Complete(agentID, message string) error {
	return c.manager.Complete(agentID, message)
}

func (c *AgentControl) Fail(agentID, message string) error {
	return c.manager.Fail(agentID, message)
}

func (c *AgentControl) Settle(result Result) error {
	return c.manager.Settle(result)
}

func (c *AgentControl) Result(agentID string) (Result, bool) {
	return c.manager.Result(agentID)
}

func (c *AgentControl) IntegrationResult(agentID string) (Result, bool) {
	return c.manager.IntegrationResult(agentID)
}

func (c *AgentControl) SaveIntegration(candidate IntegrationCandidate) error {
	return c.manager.SaveIntegration(candidate)
}

func (c *AgentControl) Integration(
	agentID, previewDigest string,
) (IntegrationCandidate, bool, error) {
	return c.manager.Integration(agentID, previewDigest)
}

func (c *AgentControl) BeginIntegration(agentID string) error {
	return c.manager.BeginIntegration(agentID)
}

func (c *AgentControl) FinishIntegration(agentID string, err error) error {
	return c.manager.FinishIntegration(agentID, err)
}

func (c *AgentControl) WriteOwner(path string) (string, bool) {
	return c.manager.WriteOwner(path)
}

func (c *AgentControl) Mailbox() *Mailbox {
	return c.manager.Mailbox()
}

func (c *AgentControl) AttachGraph(graph Graph) error {
	return c.manager.AttachGraph(graph)
}
