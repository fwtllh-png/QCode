package model

import (
	"os"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

func TestDecisionTableRows(t *testing.T) {
	agentLifecycle := Effect{
		Kind: AgentLifecycle, Risk: RiskMedium,
		Reversibility: Bounded,
	}
	derived := func(
		capability Capability, access Access, strong bool,
		resources ...Resource,
	) AssessmentInput {
		return AssessmentInput{
			Binding: AssessmentBinding{
				Capability: capability, Access: access,
				SandboxDeclared: true, StrongSandbox: strong,
			},
			Resources: resources,
		}
	}
	undeclared := func(capability Capability) AssessmentInput {
		return AssessmentInput{Binding: AssessmentBinding{Capability: capability}}
	}
	fullAccess := derived(CapabilityProcess, Read, true)
	fullAccess.Declared.FullAccess = true
	host := derived(CapabilityProcess, Read, true)
	host.Declared.HostExecution = true
	tests := []struct {
		name  string
		input AssessmentInput
		rule  string
		want  Effect
	}{
		{
			name: "declared read-only spawn",
			input: AssessmentInput{
				Binding:  AssessmentBinding{Capability: CapabilityWrite, Fixed: &agentLifecycle},
				Declared: Declared{ReadOnly: true},
			},
			rule: RuleDeclaredReadOnly,
			want: classified(AgentLifecycle, RiskLow, Reversible),
		},
		{
			name:  "fixed classification",
			input: AssessmentInput{Binding: AssessmentBinding{Capability: CapabilityWrite, Fixed: &agentLifecycle}},
			rule:  RuleFixed,
			want:  agentLifecycle,
		},
		{
			name:  "undeclared read",
			input: undeclared(CapabilityRead),
			rule:  RuleUndeclaredRead,
			want:  classified(WorkspaceRead, RiskLow, Reversible),
		},
		{
			name:  "undeclared write",
			input: undeclared(CapabilityWrite),
			rule:  RuleUndeclaredWrite,
			want:  classified(ExternalMutation, RiskMedium, Bounded),
		},
		{
			name:  "undeclared process",
			input: undeclared(CapabilityProcess),
			rule:  RuleUndeclaredEffectful,
			want:  classified(ExternalMutation, RiskHigh, Irreversible),
		},
		{
			name:  "undeclared unknown capability",
			input: undeclared("teleport"),
			rule:  RuleUndeclaredUnknown,
			want:  classified(ExternalMutation, RiskCritical, Irreversible),
		},
		{
			name: "read capability",
			input: derived(CapabilityRead, Read, false,
				path("a.go", Read)),
			rule: RuleRead,
			want: classified(WorkspaceRead, RiskLow, Reversible),
		},
		{
			name: "plan only write",
			input: derived(CapabilityWrite, Write, false,
				named(ClassPlan, "session", Write)),
			rule: RulePlanOnly,
			want: classified(SessionMutation, RiskLow, Reversible),
		},
		{
			name: "agent resource",
			input: derived(CapabilityWrite, Write, false,
				named(ClassAgent, "agent-1", Write)),
			rule: RuleAgent,
			want: classified(AgentLifecycle, RiskHigh, Bounded),
		},
		{name: "host process", input: host, rule: RuleProcessHost, want: classified(ProcessMutating, RiskHigh, Irreversible)},
		{name: "full access process", input: fullAccess, rule: RuleProcessFullAccess, want: classified(ProcessMutating, RiskHigh, Irreversible)},
		{
			name: "strong process reaching only loopback",
			input: derived(CapabilityProcess, Tree, true,
				Resource{Class: ClassLoopback, Access: Write}),
			rule: RuleLoopbackOnly,
			want: classified(NetworkRead, RiskMedium, Bounded),
		},
		{
			name: "network read",
			input: derived(CapabilityNetwork, Read, false,
				network("https", "example.com", 443, Read)),
			rule: RuleNetworkRead,
			want: classified(NetworkRead, RiskMedium, Bounded),
		},
		{
			name: "process plaintext safe methods",
			input: derived(CapabilityProcess, Read, true,
				network("http", "example.com", 80, Write, "GET", "HEAD")),
			rule: RuleNetworkRead,
			want: classified(NetworkRead, RiskMedium, Bounded),
		},
		{
			name: "process https tunnel carries data",
			input: derived(CapabilityProcess, Read, true,
				network("https", "example.com", 443, Write, "CONNECT")),
			rule: RuleNetworkMutating,
			want: classified(NetworkMutating, RiskHigh, Irreversible),
		},
		{
			name: "network with workspace write",
			input: derived(CapabilityNetwork, Read, false,
				network("https", "example.com", 443, Read),
				path("out.json", Write)),
			rule: RuleNetworkMutating,
			want: classified(NetworkMutating, RiskHigh, Irreversible),
		},
		{
			name: "strong read-only process",
			input: derived(CapabilityProcess, Tree, true,
				named(ClassProcess, "workspace", Read)),
			rule: RuleProcessReadOnly,
			want: classified(ProcessReadOnly, RiskLow, Reversible),
		},
		{
			name: "process repository-tree write",
			input: derived(CapabilityProcess, Read, true,
				Resource{Class: ClassPath, Path: ".", Tree: true, Access: Write}),
			rule: RuleProcessMutating,
			want: classified(ProcessMutating, RiskHigh, Bounded),
		},
		{
			name: "journaled workspace edit",
			input: AssessmentInput{
				Binding: AssessmentBinding{
					Capability: CapabilityWrite, Access: Write,
					SandboxDeclared: true, Journaled: true,
				},
				Resources: []Resource{path("a.go", Write)},
			},
			rule: RuleJournaledEdit,
			want: classified(WorkspaceEdit, RiskLow, Reversible),
		},
		{
			name:  "external fallback",
			input: derived(CapabilityExternal, Tree, true),
			rule:  RuleExternal,
			want:  classified(ExternalMutation, RiskHigh, Irreversible),
		},
	}
	covered := map[string]bool{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Assess(test.input)
			if got.Rule() != test.rule || got.Effect() != test.want {
				t.Fatalf("assessment = %s %+v, want %s %+v",
					got.Rule(), got.Effect(), test.rule, test.want)
			}
			if err := got.Effect().Validate(); err != nil {
				t.Fatal(err)
			}
		})
		covered[test.rule] = true
	}
	for _, rule := range Rules() {
		if !covered[rule] {
			t.Errorf("decision table row %s has no test", rule)
		}
	}
}

func TestDecisionTableMatchesSecurityDocument(t *testing.T) {
	document, err := os.ReadFile("../../../docs/zh-CN/security.md")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, line := range strings.Split(string(document), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		cell := strings.Trim(strings.TrimSpace(cells[2]), "`")
		for _, rule := range Rules() {
			if cell == rule {
				documented = append(documented, cell)
			}
		}
	}
	if strings.Join(documented, ",") != strings.Join(Rules(), ",") {
		t.Fatalf("security.md decision table rows = %v, want %v", documented, Rules())
	}
}

func TestFacets(t *testing.T) {
	got := Assess(AssessmentInput{
		Binding: AssessmentBinding{
			Capability: CapabilityProcess, Access: Read,
			SandboxDeclared: true, StrongSandbox: true, PlanningExempt: true,
		},
		Declared: Declared{Verification: true},
		Resources: []Resource{
			{Class: ClassLoopback, Access: Write},
			network("http", "mirror.example.com", 80, Read, "GET"),
			{Class: ClassNetwork, ID: "not a target", Access: Read},
			named(ClassProcess, "workspace", Read),
			{Class: ClassPath, Path: ".", Tree: true, Access: Read},
		},
	}).Facets()
	want := Facets{
		Egress: EgressMutating, Network: true, LoopbackReach: true,
		HostLocalTarget: true, Process: true, StrongSandbox: true,
		DeclaredVerification: true, PlanningExempt: true,
	}
	if got != want {
		t.Fatalf("facets = %+v, want %+v", got, want)
	}
	safe := Assess(AssessmentInput{Resources: []Resource{
		network("http", "mirror.example.com", 80, Read, "get"),
	}}).Facets()
	if safe.Egress != EgressSafeRead || safe.HostLocalTarget {
		t.Fatalf("safe facets = %+v", safe)
	}
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "localhost.", "app.localhost"} {
		local := Assess(AssessmentInput{Resources: []Resource{
			network("http", host, 80, Read),
		}}).Facets()
		if !local.HostLocalTarget {
			t.Fatalf("%s is not host-local: %+v", host, local)
		}
	}
	plan := Assess(AssessmentInput{Resources: []Resource{
		named(ClassPlan, "session", Write),
		path("notes.md", Read),
	}}).Facets()
	if !plan.PlanOnly {
		t.Fatalf("plan facets = %+v", plan)
	}
	mixed := Assess(AssessmentInput{Resources: []Resource{
		named(ClassPlan, "session", Write),
		{Class: ClassLoopback, Access: Write},
	}}).Facets()
	if mixed.PlanOnly {
		t.Fatalf("plan with loopback is plan-only: %+v", mixed)
	}
}

func TestDigestBindsEffectAndFacets(t *testing.T) {
	input := AssessmentInput{
		Binding: AssessmentBinding{
			Capability: CapabilityProcess, Access: Read,
			SandboxDeclared: true, StrongSandbox: true,
		},
		Resources: []Resource{named(ClassProcess, "workspace", Read)},
	}
	base := Assess(input)
	if base.Digest() != Assess(input).Digest() {
		t.Fatal("digest is not deterministic")
	}
	verified := input
	verified.Declared.Verification = true
	if Assess(verified).Digest() == base.Digest() {
		t.Fatal("digest ignores facets")
	}
	writing := input
	writing.Resources = append(writing.Resources, path("a.go", Write))
	if Assess(writing).Digest() == base.Digest() {
		t.Fatal("digest ignores the effect")
	}
}

func path(value string, access Access) Resource {
	return Resource{Class: ClassPath, Path: value, Access: access}
}

func named(class ResourceClass, id string, access Access) Resource {
	return Resource{Class: class, ID: id, Access: access}
}

func network(
	scheme, host string, port uint16, access Access, methods ...string,
) Resource {
	return Resource{
		Class: ClassNetwork, Access: access, Methods: methods,
		Network: &netpolicy.Target{Scheme: scheme, Host: host, Port: port},
	}
}
