package policy

import (
	"path/filepath"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	securitypaths "github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

// Layer names the evaluation layer that produced a Decision. Layers are
// evaluated in the order of Layers; the first terminal verdict wins.
type Layer string

const (
	LayerInput      Layer = "input"
	LayerHard       Layer = "hard_constraint"
	LayerRepository Layer = "repository"
	LayerUser       Layer = "user"
	LayerMode       Layer = "mode"
	LayerPosture    Layer = "posture"
	LayerSurface    Layer = "surface"
	LayerBinding    Layer = "binding"
	LayerAutoReview Layer = "auto_review"
)

func Layers() []Layer {
	return []Layer{
		LayerInput, LayerHard, LayerRepository, LayerUser, LayerMode,
		LayerPosture, LayerSurface, LayerBinding, LayerAutoReview,
	}
}

// ApprovalRequirement tells the approval flow how an Ask may be satisfied.
type ApprovalRequirement string

const (
	// ApprovalReusable accepts a matching cached approval.
	ApprovalReusable ApprovalRequirement = ""
	// ApprovalFresh ignores cached approvals.
	ApprovalFresh ApprovalRequirement = "fresh"
	// ApprovalFreshOnce ignores cached approvals, offers only a once scope,
	// forbids argument replacement, and is never cached.
	ApprovalFreshOnce ApprovalRequirement = "fresh_once"
)

// Stage distinguishes admitting a call from approving a network target the
// admitted call reached at runtime. Binding approval policy governs only
// admission.
type Stage string

const (
	StageCall   Stage = ""
	StageEgress Stage = "egress_target"
)

// Decide is the single policy entry point. It evaluates a sampled snapshot
// so concurrent configuration changes cannot mix into one decision.
func (r *Runtime) Decide(invocation Invocation) Decision {
	if r == nil {
		return deny("policy_unavailable", "security runtime is required").at(LayerInput)
	}
	return r.CloneSampling().decide(invocation)
}

func (r *Runtime) decide(invocation Invocation) Decision {
	assessment := invocation.Assessment
	if decision, done := inputLayer(invocation, assessment); done {
		return decision
	}
	if decision, done := controlPlaneLayer(invocation, assessment); done {
		return decision
	}
	if rule, ok := strongestMatch(r.Constitution, invocation); ok {
		return deny(ruleCode(rule, "constitution_denied"), "constitution rule matched").at(LayerHard)
	}
	grant, ok := strongestMatch(r.Grants, invocation)
	if !ok {
		return deny("tool_grant_missing", "no matching managed tool grant").at(LayerHard)
	}
	if grant.Action == ActionDeny || grant.Action == ActionHold {
		return deny("tool_grant_denied", "managed tool grant denied this invocation").at(LayerHard)
	}
	var askLayer Layer
	if grant.Action == ActionAsk {
		askLayer = LayerHard
	}
	repositoryAsk := false
	if rule, ok := strongestMatch(r.Repository, invocation); ok {
		switch rule.Action {
		case ActionDeny:
			return deny("repository_rule_denied", "repository deny rule matched").at(LayerRepository)
		case ActionHold:
			return deny(ruleCode(rule, "repository_hold"), "repository mechanical hold matched").at(LayerRepository)
		case ActionAsk:
			repositoryAsk = true
			askLayer = firstLayer(askLayer, LayerRepository)
		case ActionAllow:
			return deny("repository_source_invalid", "repository authority cannot allow").at(LayerRepository)
		}
	}
	userAllow := false
	if rule, ok := strongestMatch(r.User, invocation); ok {
		switch rule.Action {
		case ActionDeny, ActionHold:
			return deny("user_rule_denied", "user authority denied this invocation").at(LayerUser)
		case ActionAsk:
			askLayer = firstLayer(askLayer, LayerUser)
		case ActionAllow:
			userAllow = true
		}
	}
	if err := validateMode(r.Mode); err != nil {
		return decisionFromError(err).at(LayerMode)
	}
	if planning := planningDecision(r, assessment); planning != nil {
		// A planning Ask is already the most specific approval; posture,
		// surface, and auto review do not revisit it.
		if planning.Action != ActionAsk {
			return planning.at(LayerMode)
		}
		return r.bindingLayer(invocation, planning.at(LayerMode))
	}
	permission, err := permissionDecision(r.Permission, invocation.Capability(), assessment.Effect())
	if err != nil {
		return decisionFromError(err).at(LayerPosture)
	}
	decision := Decision{Action: ActionAllow, Layer: LayerPosture}
	switch {
	case askLayer != "":
		decision = approvalRequired(askLayer)
	case permission == ActionAsk && userAllow:
		decision.Layer = LayerUser
	case permission == ActionAsk:
		decision = approvalRequired(LayerPosture)
	}
	review := autoReviewInput{
		runtime: r, invocation: invocation, assessment: assessment,
		permission: permission, repositoryAsk: repositoryAsk,
		managedAsk: grant.Action == ActionAsk,
	}
	eligible := autoReviewEligible(review)
	surface := ClassifySurface(invocation.Source, invocation.Capability())
	if tightened := ApplySurfaceTightening(decision, surface, r.Granular, assessment.Effect()); tightened != decision {
		decision = tightened.at(LayerSurface)
	} else if eligible {
		// Auto review would allow; the surface posture judges that allow.
		if tightened := ApplySurfaceTightening(Decision{Action: ActionAllow}, surface, r.Granular, assessment.Effect()); tightened.Action != ActionAllow {
			decision = tightened.at(LayerSurface)
		}
	}
	decision = r.bindingLayer(invocation, decision)
	if eligible && decision.Action == ActionAsk &&
		(decision.Layer == LayerPosture || decision.Layer == LayerUser) {
		decision = Decision{
			Action: ActionAllow, Code: "auto_review_allowed",
			Reason: "bounded medium-risk effect has an exact typed grant",
			Layer:  LayerAutoReview,
		}
	}
	return decision
}

func inputLayer(invocation Invocation, assessment securitymodel.Assessment) (Decision, bool) {
	var decision Decision
	switch {
	case invocation.CallID == "" || invocation.Tool == "":
		decision = deny("policy_invalid_invocation", "call id and tool are required")
	case !invocation.Validated:
		decision = deny("policy_unvalidated_invocation", "schema and resources must be validated before policy")
	case !assessment.Valid():
		decision = deny("policy_unassessed_invocation", "resolved resource assessment is required")
	case !invocation.Source.Valid():
		decision = deny("policy_invalid_invocation", "known trusted invocation source is required")
	case invocation.Capability() == "":
		decision = deny("policy_unknown_capability", "descriptor capability is required")
	case invocation.Stage != StageCall && invocation.Stage != StageEgress:
		decision = deny("policy_invalid_invocation", "unknown decision stage")
	case invocation.Approval != "" &&
		invocation.Approval != securitymodel.ApprovalDefault &&
		invocation.Approval != securitymodel.ApprovalOnce:
		decision = deny("policy_invalid_invocation", "unknown binding approval policy")
	case writesPaths(assessment.Resources()) && !canonicalRoot(invocation.Workspace):
		decision = deny("policy_invalid_invocation", "path writes require the canonical workspace root")
	default:
		return Decision{}, false
	}
	return decision.at(LayerInput), true
}

// controlPlaneLayer rejects writes to protected metadata before any rule can
// allow or ask for them; no approval can authorize these writes.
func controlPlaneLayer(invocation Invocation, assessment securitymodel.Assessment) (Decision, bool) {
	if !writesPaths(assessment.Resources()) {
		return Decision{}, false
	}
	classifier, err := securitypaths.ControlPlaneWithin(invocation.Workspace)
	if err != nil {
		return deny("policy_invalid_invocation", err.Error()).at(LayerInput), true
	}
	for _, resource := range assessment.Resources() {
		if !pathWrite(resource) {
			continue
		}
		if err := classifier.CheckWrite(resource.Path, resource.Tree); err != nil {
			location := resource.Path
			if !filepath.IsAbs(location) {
				location = filepath.Join(invocation.Workspace, location)
			}
			decision := deny("control_plane_protected", err.Error()).at(LayerHard)
			decision.Resource = location
			return decision, true
		}
	}
	return Decision{}, false
}

func (r *Runtime) bindingLayer(invocation Invocation, decision Decision) Decision {
	if invocation.Stage == StageEgress ||
		decision.Action == ActionDeny || decision.Action == ActionHold {
		return decision
	}
	if invocation.Approval == securitymodel.ApprovalOnce {
		return Decision{
			Action: ActionAsk, Code: "host_process_approval_required",
			Reason: "host process execution requires one-time user approval",
			Layer:  LayerBinding, Approval: ApprovalFreshOnce,
		}
	}
	if !r.ForceEditPlanApproval {
		return decision
	}
	if invocation.Journaled() && decision.Action == ActionAllow {
		return Decision{
			Action: ActionAsk, Code: "edit_plan_required",
			Reason: "workspace writes require a fresh edit plan approval",
			Layer:  LayerBinding, Approval: ApprovalFresh,
		}
	}
	if decision.Action == ActionAsk {
		decision.Approval = ApprovalFresh
	}
	return decision
}

type autoReviewInput struct {
	runtime       *Runtime
	invocation    Invocation
	assessment    securitymodel.Assessment
	permission    Action
	repositoryAsk bool
	managedAsk    bool
}

type autoReviewCondition struct {
	name  string
	holds func(autoReviewInput) bool
}

// autoReviewConditions must all hold for an Ask to be allowed without a
// human. Loopback services and cloud metadata are never auto-reviewed:
// model input naming them is the classic request-forgery path.
var autoReviewConditions = []autoReviewCondition{
	{"auto_review_enabled", func(in autoReviewInput) bool {
		return !in.runtime.DisableAutoReview
	}},
	{"posture_requested_approval", func(in autoReviewInput) bool {
		return in.permission == ActionAsk && !in.repositoryAsk && !in.managedAsk
	}},
	{"medium_risk", func(in autoReviewInput) bool {
		return in.assessment.Effect().Risk == securitymodel.RiskMedium
	}},
	{"bounded_effect", func(in autoReviewInput) bool {
		kind := in.assessment.Effect().Kind
		return kind == securitymodel.AgentLifecycle || kind == securitymodel.NetworkRead
	}},
	{"network_read_under_auto", func(in autoReviewInput) bool {
		return in.assessment.Effect().Kind != securitymodel.NetworkRead ||
			in.runtime.Permission == PermissionAuto
	}},
	{"public_network_target", func(in autoReviewInput) bool {
		facets := in.assessment.Facets()
		return in.assessment.Effect().Kind != securitymodel.NetworkRead ||
			(!facets.HostLocalTarget && !facets.LoopbackReach)
	}},
	{"reusable_binding_approval", func(in autoReviewInput) bool {
		return in.invocation.Stage == StageEgress ||
			(in.invocation.Approval != securitymodel.ApprovalOnce &&
				!(in.runtime.ForceEditPlanApproval && in.invocation.Journaled()))
	}},
	{"exact_typed_grant", func(in autoReviewInput) bool {
		_, typed := GrantForInvocation(in.invocation)
		return typed
	}},
}

// AutoReviewConditions lists the auto-review predicates in evaluation order.
func AutoReviewConditions() []string {
	names := make([]string, 0, len(autoReviewConditions))
	for _, condition := range autoReviewConditions {
		names = append(names, condition.name)
	}
	return names
}

func autoReviewEligible(in autoReviewInput) bool {
	for _, condition := range autoReviewConditions {
		if !condition.holds(in) {
			return false
		}
	}
	return true
}

func (d Decision) at(layer Layer) Decision {
	d.Layer = layer
	return d
}

func approvalRequired(layer Layer) Decision {
	return Decision{
		Action: ActionAsk, Code: "approval_required",
		Reason: "approval is required", Layer: layer,
	}
}

func firstLayer(current, next Layer) Layer {
	if current != "" {
		return current
	}
	return next
}

func ruleCode(rule Rule, fallback string) string {
	if rule.Code != "" {
		return rule.Code
	}
	return fallback
}

func pathWrite(resource securitymodel.Resource) bool {
	return resource.Class == securitymodel.ClassPath && resource.Writes() && resource.Path != ""
}

func writesPaths(resources []securitymodel.Resource) bool {
	for _, resource := range resources {
		if pathWrite(resource) {
			return true
		}
	}
	return false
}

func canonicalRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root
}
