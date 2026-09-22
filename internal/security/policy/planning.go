package policy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type PlanningPolicy string

const (
	PlanningOff      PlanningPolicy = "off"
	PlanningAdaptive PlanningPolicy = "adaptive"
	PlanningRequired PlanningPolicy = "required"
)

type PlanningSnapshot struct {
	Planning      string `json:"planning,omitempty"`
	PlanSubmitted bool   `json:"plan_submitted,omitempty"`
}

func (r *Runtime) PlanningSnapshot() PlanningSnapshot {
	if r == nil {
		return PlanningSnapshot{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return PlanningSnapshot{
		Planning:      string(r.PlanningPolicy),
		PlanSubmitted: r.PlanSubmitted,
	}
}

func (s PlanningSnapshot) Guidance() string {
	if s.Planning == "" || s.Planning == string(PlanningOff) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- planning=%s submitted=%t\n",
		s.Planning, s.PlanSubmitted)
	if s.Planning == string(PlanningRequired) {
		b.WriteString("- submit_plan is required before consequential actions\n")
	} else {
		b.WriteString("- use submit_plan before complex or high-risk actions\n")
	}
	return b.String()
}

func (p SurfacePosture) Label() string {
	if p == "" {
		return "inherit"
	}
	return string(p)
}

func (r *Runtime) ConfigurePlanning(
	planning PlanningPolicy,
) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanningPolicy = planning
	r.PlanSubmitted = false
	return r.bumpRevisionLocked()
}

func (r *Runtime) SubmitPlan() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanSubmitted = true
	return r.bumpRevisionLocked()
}

func (r *Runtime) ResetPlanState() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanSubmitted = false
	return r.bumpRevisionLocked()
}

func planningDecision(
	r *Runtime,
	invocation Invocation,
	effect Effect,
) *Decision {
	if r == nil || r.Mode == ModePlan ||
		planningExemptTool(invocation.Tool) ||
		!consequentialPlanningEffect(effect.Kind) {
		return nil
	}
	if r.PlanningPolicy != PlanningOff &&
		r.PlanningPolicy != PlanningAdaptive &&
		r.PlanningPolicy != PlanningRequired {
		return &Decision{
			Action: ActionDeny, Code: "planning_policy_invalid",
			Reason: "unknown planning policy is denied",
		}
	}
	if r.PlanningPolicy == PlanningOff {
		return nil
	}
	required := r.PlanningPolicy == PlanningRequired ||
		(r.PlanningPolicy == PlanningAdaptive &&
			adaptivePlanningRequired(effect, invocation))
	if !required && !r.PlanSubmitted {
		return nil
	}
	if !r.PlanSubmitted {
		if declaredVerification(invocation) {
			// Verification is the checking step of a plan: downgrade the
			// gate from a hard Hold to one approval instead of forcing a
			// plan re-submission before every build or test run.
			return &Decision{
				Action: ActionAsk, Code: "plan_verification",
				Reason: "approve this declared verification run, or submit_plan first",
			}
		}
		return &Decision{
			Action: ActionHold, Code: "plan_required",
			Reason: "submit a structured Plan before consequential actions",
		}
	}
	return nil
}

// declaredVerification reports an exec_command invocation that declared
// verification semantics (kind plus exact covered paths). The declaration
// is schema-validated before policy runs, so both fields are trustworthy.
func declaredVerification(invocation Invocation) bool {
	if invocation.Tool != "exec_command" {
		return false
	}
	var payload struct {
		Verification string   `json:"verification"`
		CoveredPaths []string `json:"covered_paths"`
	}
	if json.Unmarshal(invocation.Arguments, &payload) != nil {
		return false
	}
	return strings.TrimSpace(payload.Verification) != "" &&
		len(payload.CoveredPaths) != 0
}

func planningExemptTool(name string) bool {
	switch name {
	case "git_push":
		return true
	default:
		return false
	}
}

func validatePlanning(planning PlanningPolicy) error {
	if planning != PlanningOff && planning != PlanningAdaptive &&
		planning != PlanningRequired {
		return fmt.Errorf("unknown planning policy %q", planning)
	}
	return nil
}

func consequentialPlanningEffect(kind EffectKind) bool {
	switch kind {
	case EffectWorkspaceRead, EffectProcessReadOnly,
		EffectSessionMutation, EffectAgentMessage:
		return false
	default:
		return true
	}
}

func adaptivePlanningRequired(effect Effect, invocation Invocation) bool {
	if readOnlySpawn(invocation) {
		return false
	}
	return effect.Risk == RiskHigh || effect.Risk == RiskCritical ||
		effect.Kind == EffectNetworkMutating ||
		effect.Kind == EffectExternalMutation ||
		effect.Kind == EffectAgentLifecycle ||
		effect.Reversibility == string(tool.Irreversible)
}

func readOnlySpawn(invocation Invocation) bool {
	if invocation.Tool != "spawn_agent" {
		return false
	}
	var payload struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(invocation.Arguments, &payload) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(payload.Role)) {
	case "review", "explore", "awaiter":
		return true
	default:
		return false
	}
}
