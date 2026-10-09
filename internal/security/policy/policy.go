package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

type Mode string

const ModeAct Mode = "act"

type Permission string

const (
	PermissionSuggest Permission = "suggest"
	PermissionAuto    Permission = "auto"
	PermissionBypass  Permission = "bypass"
	PermissionNever   Permission = "never"
)

type Capability = securitymodel.Capability

const (
	CapabilityRead     = securitymodel.CapabilityRead
	CapabilityWrite    = securitymodel.CapabilityWrite
	CapabilityProcess  = securitymodel.CapabilityProcess
	CapabilityNetwork  = securitymodel.CapabilityNetwork
	CapabilityExternal = securitymodel.CapabilityExternal
)

type Action string

const (
	ActionAllow Action = "allow"
	ActionAsk   Action = "ask"
	ActionDeny  Action = "deny"
	ActionHold  Action = "hold"
)

type Invocation struct {
	CallID, Tool string
	Source       securitymodel.SourceKind
	Arguments    json.RawMessage
	Assessment   securitymodel.Assessment
	Approval     securitymodel.ApprovalMode
	Validated    bool
	// Workspace is the canonical workspace root; path writes are classified
	// against it.
	Workspace string
	Stage     Stage
}

func (i Invocation) Capability() securitymodel.Capability { return i.Assessment.Binding().Capability }
func (i Invocation) Access() securitymodel.Access         { return i.Assessment.Binding().Access }
func (i Invocation) Journaled() bool                      { return i.Assessment.Facets().Journaled }
func (i Invocation) StrongSandbox() bool                  { return i.Assessment.Facets().StrongSandbox }

type Rule struct {
	Tool     string `json:"tool"`
	Resource string `json:"resource,omitempty"`
	// ResourcePath is the Guard-resolved filesystem interpretation of Resource.
	// Non-path resources always match the original Resource; this is not config.
	ResourcePath string `json:"-"`
	// RequireWrite limits resource matching to write-access resources. It is
	// compiler-internal (not config): constitution write holds must gate every
	// writer without holding reads of the same paths.
	RequireWrite  bool   `json:"-"`
	CommandPrefix string `json:"command_prefix,omitempty"`
	GrantKey      string `json:"grant_key,omitempty"`
	Action        Action `json:"action"`
	Code          string `json:"code,omitempty"`
}

type Runtime struct {
	mu                sync.RWMutex
	userSource        UserRuleSource
	userSourceVersion uint64
	Revision          uint64
	Mode              Mode
	Permission        Permission
	// DisableHostExecution preserves the delegated runtime's process boundary.
	// It is a construction-time ceiling, not a user permission setting.
	DisableHostExecution bool
	PlanningPolicy       PlanningPolicy
	PlanSubmitted        bool
	// DisableAutoReview is the fail-closed operational kill switch.
	DisableAutoReview bool
	// ForceEditPlanApproval makes every journaled write ask for a fresh
	// edit-plan approval and stops cached approvals from satisfying any ask.
	ForceEditPlanApproval    bool
	Grants, User, Repository []Rule
	// Constitution rules are hard constraints evaluated before managed grants.
	Constitution []Rule
	Approvals    *ApprovalCache
	Granular     Granular
	Now          func() time.Time
}

type Decision struct {
	Action       Action
	Code, Reason string
	Layer        Layer
	Approval     ApprovalRequirement
	// Resource is the canonical location behind a control-plane denial.
	Resource string
}

type DecisionError struct {
	Code, Reason string
}

func (e *DecisionError) Error() string {
	return e.Code + ": " + e.Reason
}

func DefaultRuntime(mode Mode, permission Permission) *Runtime {
	return &Runtime{
		Revision: 1, Mode: mode, Permission: permission,
		PlanningPolicy: PlanningOff,
		Grants:         []Rule{{Tool: "*", Resource: "*", Action: ActionAllow}},
		Approvals:      NewApprovalCache(), Now: time.Now,
	}
}

func (r *Runtime) SetPermission(permission Permission) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Permission = permission
	return r.bumpRevisionLocked()
}

func (r *Runtime) SetPermissionWithinCeiling(
	requested Permission,
	ceiling Permission,
) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ceiling == "" {
		ceiling = r.Permission
	}
	r.Permission = TightenPermission(requested, ceiling)
	return r.bumpRevisionLocked()
}

func (r *Runtime) SetGranular(granular Granular) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Granular = granular
	return r.bumpRevisionLocked()
}

func (r *Runtime) SetForceEditPlanApproval(forced bool) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ForceEditPlanApproval == forced {
		return r.Revision
	}
	r.ForceEditPlanApproval = forced
	return r.bumpRevisionLocked()
}

func (r *Runtime) SetDisableAutoReview(disabled bool) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.DisableAutoReview == disabled {
		return r.Revision
	}
	r.DisableAutoReview = disabled
	return r.bumpRevisionLocked()
}

func (r *Runtime) AppendManagedRule(rule Rule) (uint64, error) {
	if r == nil {
		return 0, errors.New("policy runtime is required")
	}
	if err := ValidateRules(SourceManaged, []Rule{rule}); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Grants = append(append([]Rule(nil), r.Grants...), rule)
	return r.bumpRevisionLocked(), nil
}

func (r *Runtime) PermissionValue() Permission {
	snapshot := r.CloneSampling()
	if snapshot == nil {
		return PermissionNever
	}
	return snapshot.Permission
}

func (r *Runtime) bumpRevisionLocked() uint64 {
	if r.Revision == 0 {
		r.Revision = 1
	}
	r.Revision++
	return r.Revision
}

func TightenPermission(requested, ceiling Permission) Permission {
	ranks := map[Permission]int{
		PermissionNever: 0, PermissionSuggest: 1,
		PermissionAuto: 2, PermissionBypass: 3,
	}
	requestedRank, requestedOK := ranks[requested]
	ceilingRank, ceilingOK := ranks[ceiling]
	if !requestedOK || !ceilingOK {
		return PermissionNever
	}
	if requestedRank > ceilingRank {
		return ceiling
	}
	return requested
}

// CloneSampling copies policy state while sharing session grants and clocks.
func (r *Runtime) CloneSampling() *Runtime {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshUserRulesLocked()
	return &Runtime{
		Revision: r.Revision, Mode: r.Mode, Permission: r.Permission,
		DisableHostExecution:  r.DisableHostExecution,
		PlanningPolicy:        r.PlanningPolicy,
		PlanSubmitted:         r.PlanSubmitted,
		DisableAutoReview:     r.DisableAutoReview,
		ForceEditPlanApproval: r.ForceEditPlanApproval,
		Grants:                append([]Rule(nil), r.Grants...),
		User:                  append([]Rule(nil), r.User...),
		Repository:            append([]Rule(nil), r.Repository...),
		Constitution:          append([]Rule(nil), r.Constitution...),
		Approvals:             r.Approvals, Granular: r.Granular, Now: r.Now,
	}
}

func (r *Runtime) ManagedGrant(invocation Invocation) (Rule, bool) {
	if r == nil {
		return Rule{}, false
	}
	snapshot := r.CloneSampling()
	return strongestMatch(snapshot.Grants, invocation)
}

// AdvertisesTool reports whether a tool should appear in the model catalog.
// Prefix or resource-scoped denies do not hide the whole tool; only a matching
// blanket managed deny/hold does.
func (r *Runtime) AdvertisesTool(name string) bool {
	if r == nil || name == "" {
		return true
	}
	grant, ok := r.ManagedGrant(Invocation{Tool: name, Validated: true})
	if !ok {
		return true
	}
	return grant.Action != ActionDeny && grant.Action != ActionHold
}

func deny(code, reason string) Decision {
	return Decision{Action: ActionDeny, Code: code, Reason: reason}
}

func decisionFromError(err error) Decision {
	var decision *DecisionError
	if errors.As(err, &decision) {
		return deny(decision.Code, decision.Reason)
	}
	return deny("policy_denied", err.Error())
}

func validateMode(mode Mode) error {
	if mode != ModeAct {
		return decisionError("mode_unknown", "only act mode is supported")
	}
	return nil
}

func permissionDecision(
	permission Permission,
	capability securitymodel.Capability,
	eff securitymodel.Effect,
) (Action, error) {
	if permission != PermissionSuggest && permission != PermissionAuto &&
		permission != PermissionBypass && permission != PermissionNever {
		return ActionDeny, decisionError("permission_unknown", "unknown permission is denied")
	}
	if permission == PermissionNever {
		if capability == securitymodel.CapabilityRead ||
			eff.Kind == securitymodel.ProcessReadOnly {
			return ActionAllow, nil
		}
		return ActionDeny, decisionError("permission_denied", "never posture denies side effects")
	}
	if eff.Risk == securitymodel.RiskCritical {
		return ActionDeny, decisionError("permission_denied", "critical-risk execution is denied")
	}
	if permission == PermissionBypass || capability == securitymodel.CapabilityRead ||
		eff.Risk == securitymodel.RiskLow {
		return ActionAllow, nil
	}
	return ActionAsk, nil
}

func strongestMatch(rules []Rule, invocation Invocation) (Rule, bool) {
	var strongest Rule
	found := false
	for _, rule := range rules {
		if ruleMatches(rule, invocation) &&
			(!found || actionPriority(rule.Action) > actionPriority(strongest.Action)) {
			strongest, found = rule, true
		}
	}
	return strongest, found
}

func ruleMatches(rule Rule, invocation Invocation) bool {
	if rule.Tool != "" && rule.Tool != "*" && rule.Tool != invocation.Tool {
		return false
	}
	if rule.Resource != "" && rule.Resource != "*" {
		matched := false
		// An unrestricted process can reach any host path or network target.
		// It cannot evade a scoped restriction by omitting resources. A caller
		// can select exact write/network scopes for a narrower authorization.
		if invocation.Assessment.Facets().FullAccess || invocation.Assessment.Facets().HostExecution {
			// A scoped allow is not a grant for an unrestricted process, even
			// when one of its covered paths happens to match that allow.
			if rule.Action == ActionAllow {
				return false
			}
			matched = true
		}
		for _, resource := range invocation.Assessment.Resources() {
			if rule.RequireWrite && !resource.Access.Writes() {
				continue
			}
			value := resource.Path
			pattern := rule.Resource
			if value == "" {
				value = resource.Location()
				if resource.Network != nil {
					value = resource.Network.Host
				}
				if resource.URL != "" {
					value = resource.URL
				}
			} else if rule.ResourcePath != "" {
				pattern = rule.ResourcePath
			}
			tree := resource.Writes() && resource.Tree
			if resourceMatchesAuthority(pattern, value, tree, rule.Action != ActionAllow) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if rule.CommandPrefix != "" {
		var input struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(invocation.Arguments, &input) != nil ||
			!commandRuleMatches(input.Command, rule.CommandPrefix, rule.Action) {
			return false
		}
	}
	if rule.GrantKey != "" {
		grant, ok := GrantForInvocation(invocation)
		if !ok || grant.Key != rule.GrantKey {
			return false
		}
	}
	return true
}

// resourceMatches applies a path pattern to path-like rules and literal
// subtree matching to identifiers such as URLs. Validation rejects malformed
// path patterns, so a pattern that fails to compile here matches nothing.
func resourceMatches(pattern, value string) bool {
	return resourceMatchesAuthority(pattern, value, false, false)
}

func resourceMatchesAuthority(pattern, value string, tree, restrictive bool) bool {
	if IsPathPattern(pattern) {
		compiled, err := CompilePathPattern(pattern)
		if err != nil {
			return restrictive
		}
		if tree {
			return compiled.IntersectsTree(value)
		}
		return compiled.Match(value)
	}
	value = filepath.ToSlash(filepath.Clean(value))
	pattern = filepath.ToSlash(filepath.Clean(pattern))
	return value == pattern || strings.HasPrefix(value, strings.TrimSuffix(pattern, "/")+"/")
}

func actionPriority(action Action) int {
	return map[Action]int{
		ActionAllow: 1, ActionAsk: 2, ActionDeny: 3, ActionHold: 4,
	}[action]
}

func decisionError(code, reason string) error {
	return &DecisionError{Code: code, Reason: reason}
}

func Validate(runtime *Runtime) error {
	if runtime == nil {
		return errors.New("runtime is required")
	}
	runtime = runtime.CloneSampling()
	if err := validateMode(runtime.Mode); err != nil {
		return fmt.Errorf("mode: %w", err)
	}
	if _, err := permissionDecision(runtime.Permission, securitymodel.CapabilityRead, securitymodel.Effect{
		Kind: securitymodel.WorkspaceRead, Risk: securitymodel.RiskLow, Reversibility: securitymodel.Reversible,
	}); err != nil {
		return fmt.Errorf("permission: %w", err)
	}
	if err := validatePlanning(runtime.PlanningPolicy); err != nil {
		return err
	}
	if err := ValidateRules(SourceManaged, runtime.Grants); err != nil {
		return err
	}
	if err := ValidateRules(SourceUser, runtime.User); err != nil {
		return err
	}
	if err := ValidateRules(SourceRepository, runtime.Repository); err != nil {
		return err
	}
	if err := ValidateRules(SourceConstitution, runtime.Constitution); err != nil {
		return err
	}
	return nil
}
