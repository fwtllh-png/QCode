package policy

import (
	"fmt"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
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
	assessment securitymodel.Assessment,
) *Decision {
	eff := assessment.Effect()
	if r == nil ||
		assessment.Facets().PlanningExempt ||
		!consequentialPlanningEffect(eff.Kind) {
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
	if assessment.Facets().HostExecution {
		// Host commands require a fresh command approval under Auto. Full
		// Access preauthorizes it; a separate plan gate would duplicate it.
		return nil
	}
	// Full Access already authorizes ordinary command execution. Repository/user rules are evaluated independently.
	if r.Permission == PermissionBypass &&
		assessment.Facets().FullAccess {
		return nil
	}
	required := r.PlanningPolicy == PlanningRequired ||
		(r.PlanningPolicy == PlanningAdaptive &&
			adaptivePlanningRequired(assessment))
	if !required && !r.PlanSubmitted {
		return nil
	}
	if !r.PlanSubmitted {
		return &Decision{
			Action: ActionHold, Code: "plan_required",
			Reason: "submit a structured Plan before consequential actions",
		}
	}
	return nil
}

func validatePlanning(planning PlanningPolicy) error {
	if planning != PlanningOff && planning != PlanningAdaptive &&
		planning != PlanningRequired {
		return fmt.Errorf("unknown planning policy %q", planning)
	}
	return nil
}

func consequentialPlanningEffect(kind securitymodel.EffectKind) bool {
	switch kind {
	case securitymodel.WorkspaceRead, securitymodel.ProcessReadOnly,
		securitymodel.SessionMutation, securitymodel.AgentMessage:
		return false
	default:
		return true
	}
}

func adaptivePlanningRequired(assessment securitymodel.Assessment) bool {
	if assessment.Facets().ReadOnlyDeclared {
		return false
	}
	eff := assessment.Effect()
	return eff.Risk == securitymodel.RiskHigh || eff.Risk == securitymodel.RiskCritical ||
		eff.Kind == securitymodel.NetworkMutating ||
		eff.Kind == securitymodel.ExternalMutation ||
		eff.Kind == securitymodel.AgentLifecycle ||
		eff.Reversibility == securitymodel.Irreversible
}
