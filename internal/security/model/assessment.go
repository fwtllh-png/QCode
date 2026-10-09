package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

// AssessmentBinding is the part of a trusted tool binding that classification reads.
type AssessmentBinding struct {
	Capability Capability
	// Access is the binding's declared access mode; empty when undeclared.
	Access          Access
	SandboxDeclared bool
	StrongSandbox   bool
	Journaled       bool
	// Fixed is the declared classification of a fixed-effect binding.
	Fixed          *Effect
	PlanningExempt bool
}

// Declared carries binding declarations that Guard resolved against the
// invocation's arguments.
type Declared struct {
	// HostExecution is selected by Guard from a supported command's requested
	// target. Policy must independently authorize host execution.
	HostExecution bool
	// FullAccess is selected by Guard from the session permission and a trusted
	// process binding. It is never accepted from model arguments.
	FullAccess bool
	// ReadOnly reports that an argument matched the binding's read-only values.
	ReadOnly bool
	// Verification reports a declared verification run with covered paths.
	Verification bool
}

type AssessmentInput struct {
	Binding   AssessmentBinding
	Declared  Declared
	Resources []Resource
}

// Egress is how much data the declared network targets can carry outward.
type Egress uint8

const (
	EgressNone Egress = iota
	EgressSafeRead
	EgressMutating
)

// Facets are the classification-relevant properties of an invocation.
type Facets struct {
	HostExecution   bool
	FullAccess      bool
	WritesWorkspace bool
	Egress          Egress
	// Network reports a network endpoint other than the loopback grant.
	Network         bool
	LoopbackReach   bool
	HostLocalTarget bool
	Process         bool
	Agent           bool
	// PlanOnly reports that the only effectful resource is the session plan.
	PlanOnly             bool
	StrongSandbox        bool
	Journaled            bool
	ReadOnlyDeclared     bool
	DeclaredVerification bool
	PlanningExempt       bool
}

// Assessment is the classified invocation. Rule names the decision-table row
// that produced Effect.
type Assessment struct {
	input    AssessmentInput
	facets   Facets
	effect   Effect
	rule     string
	identity string
}

// Decision-table rows, in evaluation order. docs/zh-CN/security.md documents
// the same rows in the same order.
const (
	RuleDeclaredReadOnly    = "declared_read_only"
	RuleFixed               = "fixed"
	RuleUndeclaredRead      = "undeclared_read"
	RuleUndeclaredWrite     = "undeclared_write"
	RuleUndeclaredEffectful = "undeclared_effectful"
	RuleUndeclaredUnknown   = "undeclared_unknown"
	RuleRead                = "read"
	RulePlanOnly            = "plan_only"
	RuleAgent               = "agent"
	RuleLoopbackOnly        = "loopback_only"
	RuleNetworkRead         = "network_read"
	RuleNetworkMutating     = "network_mutating"
	RuleProcessReadOnly     = "process_read_only"
	RuleProcessFullAccess   = "process_full_access"
	RuleProcessHost         = "process_host"
	RuleProcessMutating     = "process_mutating"
	RuleJournaledEdit       = "journaled_edit"
	RuleExternal            = "external"
)

type row struct {
	id      string
	matches func(AssessmentBinding, Facets) bool
	effect  func(AssessmentBinding) Effect
}

func constant(
	kind EffectKind, risk Risk, reversibility Reversibility,
) func(AssessmentBinding) Effect {
	return func(AssessmentBinding) Effect { return classified(kind, risk, reversibility) }
}

var table = []row{
	{
		RuleDeclaredReadOnly,
		func(b AssessmentBinding, f Facets) bool { return b.Fixed != nil && f.ReadOnlyDeclared },
		func(b AssessmentBinding) Effect {
			return classified(b.Fixed.Kind, RiskLow, Reversible)
		},
	},
	{
		RuleFixed,
		func(b AssessmentBinding, _ Facets) bool { return b.Fixed != nil },
		func(b AssessmentBinding) Effect { return *b.Fixed },
	},
	{
		RuleUndeclaredRead,
		func(b AssessmentBinding, _ Facets) bool { return !declared(b) && b.Capability == CapabilityRead },
		constant(WorkspaceRead, RiskLow, Reversible),
	},
	{
		RuleUndeclaredWrite,
		func(b AssessmentBinding, _ Facets) bool { return !declared(b) && b.Capability == CapabilityWrite },
		constant(ExternalMutation, RiskMedium, Bounded),
	},
	{
		RuleUndeclaredEffectful,
		func(b AssessmentBinding, _ Facets) bool {
			return !declared(b) && (b.Capability == CapabilityProcess ||
				b.Capability == CapabilityNetwork ||
				b.Capability == CapabilityExternal)
		},
		constant(ExternalMutation, RiskHigh, Irreversible),
	},
	{
		RuleUndeclaredUnknown,
		func(b AssessmentBinding, _ Facets) bool { return !declared(b) },
		constant(ExternalMutation, RiskCritical, Irreversible),
	},
	{
		RuleRead,
		func(b AssessmentBinding, _ Facets) bool { return b.Capability == CapabilityRead },
		constant(WorkspaceRead, RiskLow, Reversible),
	},
	{
		RulePlanOnly,
		func(b AssessmentBinding, f Facets) bool { return b.Capability == CapabilityWrite && f.PlanOnly },
		constant(SessionMutation, RiskLow, Reversible),
	},
	{
		RuleAgent,
		func(_ AssessmentBinding, f Facets) bool { return f.Agent },
		constant(AgentLifecycle, RiskHigh, Bounded),
	},
	{
		RuleProcessHost,
		func(b AssessmentBinding, f Facets) bool { return b.Capability == CapabilityProcess && f.HostExecution },
		constant(ProcessMutating, RiskHigh, Irreversible),
	},
	{
		RuleProcessFullAccess,
		func(b AssessmentBinding, f Facets) bool { return b.Capability == CapabilityProcess && f.FullAccess },
		constant(ProcessMutating, RiskHigh, Irreversible),
	},
	{
		// The loopback grant reaches every local port, so it is never
		// auto-reviewed even though it runs inside the Strong Sandbox.
		RuleLoopbackOnly,
		func(b AssessmentBinding, f Facets) bool {
			return b.Capability == CapabilityProcess && b.StrongSandbox &&
				f.LoopbackReach && !f.Network && !f.WritesWorkspace
		},
		constant(NetworkRead, RiskMedium, Bounded),
	},
	{
		RuleNetworkRead,
		func(b AssessmentBinding, f Facets) bool {
			return reachesNetwork(b, f) && b.Access == Read &&
				!f.WritesWorkspace &&
				!(b.Capability == CapabilityProcess && f.Egress == EgressMutating)
		},
		constant(NetworkRead, RiskMedium, Bounded),
	},
	{
		RuleNetworkMutating,
		reachesNetwork,
		constant(NetworkMutating, RiskHigh, Irreversible),
	},
	{
		RuleProcessReadOnly,
		func(b AssessmentBinding, f Facets) bool {
			return runsProcess(b, f) && b.StrongSandbox && !f.WritesWorkspace
		},
		constant(ProcessReadOnly, RiskLow, Reversible),
	},
	{
		RuleProcessMutating,
		runsProcess,
		constant(ProcessMutating, RiskHigh, Bounded),
	},
	{
		RuleJournaledEdit,
		func(b AssessmentBinding, f Facets) bool {
			return b.Capability == CapabilityWrite && f.WritesWorkspace && b.Journaled
		},
		constant(WorkspaceEdit, RiskLow, Reversible),
	},
	{
		RuleExternal,
		func(AssessmentBinding, Facets) bool { return true },
		constant(ExternalMutation, RiskHigh, Irreversible),
	},
}

// Rules lists the decision-table row identifiers in evaluation order.
func Rules() []string {
	rules := make([]string, 0, len(table))
	for _, row := range table {
		rules = append(rules, row.id)
	}
	return rules
}

func Assess(input AssessmentInput) Assessment {
	input = cloneInput(input)
	encoded, _ := json.Marshal(input)
	identity := sha256.Sum256(encoded)
	facets := facetsOf(input)
	for _, row := range table {
		if row.matches(input.Binding, facets) {
			return Assessment{
				input: input, facets: facets,
				effect: row.effect(input.Binding), rule: row.id, identity: hex.EncodeToString(identity[:]),
			}
		}
	}
	panic("assess: decision table has no terminal row")
}

// Digest fingerprints the effect and facets so reusable grants do not cross
// a change in either.
func (a Assessment) Digest() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%+v",
		a.effect.Kind, a.effect.Risk, a.effect.Reversibility, a.facets))
	return hex.EncodeToString(sum[:])
}

func facetsOf(input AssessmentInput) Facets {
	facets := Facets{
		HostExecution:        input.Declared.HostExecution,
		FullAccess:           input.Declared.FullAccess,
		StrongSandbox:        input.Binding.StrongSandbox && !input.Declared.HostExecution,
		Journaled:            input.Binding.Journaled,
		ReadOnlyDeclared:     input.Declared.ReadOnly,
		DeclaredVerification: input.Declared.Verification,
		PlanningExempt:       input.Binding.PlanningExempt,
	}
	plan := false
	for _, item := range input.Resources {
		switch item.Class {
		case ClassPath:
			facets.WritesWorkspace = facets.WritesWorkspace || item.Writes()
		case ClassNetwork:
			facets.Network = true
			host := item.ID
			if item.Network != nil {
				host = item.Network.Host
			}
			facets.HostLocalTarget = facets.HostLocalTarget || netpolicy.NamesHostLocal(host)
			if item.Network == nil || item.Network.CanCarryData(item.Methods) {
				facets.Egress = EgressMutating
			} else if facets.Egress == EgressNone {
				facets.Egress = EgressSafeRead
			}
		case ClassLoopback:
			facets.LoopbackReach = true
			facets.HostLocalTarget = true
		case ClassProcess:
			facets.Process = true
		case ClassAgent:
			facets.Agent = true
		case ClassPlan:
			plan = true
		}
	}
	facets.PlanOnly = plan && !facets.WritesWorkspace && !facets.Network &&
		!facets.LoopbackReach && !facets.Process && !facets.Agent
	return facets
}

func declared(b AssessmentBinding) bool {
	return b.Access != "" && b.SandboxDeclared
}

func reachesNetwork(b AssessmentBinding, f Facets) bool {
	return b.Capability == CapabilityNetwork || f.Network || f.LoopbackReach
}

func runsProcess(b AssessmentBinding, f Facets) bool {
	return b.Capability == CapabilityProcess || f.Process
}

func classified(
	kind EffectKind, risk Risk, reversibility Reversibility,
) Effect {
	return Effect{Kind: kind, Risk: risk, Reversibility: reversibility}
}

// Assessment owns its inputs. Getters return values or deep copies so a
// prepared invocation can share one snapshot across every security consumer.
func (a Assessment) Resources() []Resource { return CloneResources(a.input.Resources) }
func (a Assessment) Facets() Facets        { return a.facets }
func (a Assessment) Effect() Effect        { return a.effect }
func (a Assessment) Rule() string          { return a.rule }
func (a Assessment) Valid() bool           { return a.rule != "" && a.effect.Validate() == nil }

// Input is an explicit starting point for a new authorization (new resources
// or binding declarations); merely reading a snapshot never reassesses it.
func (a Assessment) Input() AssessmentInput { return cloneInput(a.input) }
func cloneInput(input AssessmentInput) AssessmentInput {
	input.Resources = CloneResources(input.Resources)
	if input.Binding.Fixed != nil {
		fixed := *input.Binding.Fixed
		input.Binding.Fixed = &fixed
	}
	return input
}

func (a Assessment) Binding() AssessmentBinding {
	binding := a.input.Binding
	if binding.Fixed != nil {
		fixed := *binding.Fixed
		binding.Fixed = &fixed
	}
	return binding
}
func (a Assessment) Declared() Declared { return a.input.Declared }

// Same reports whether both snapshots describe exactly the same resolved input.
func (a Assessment) Same(other Assessment) bool {
	return a.Valid() && other.Valid() && a.identity == other.identity
}
