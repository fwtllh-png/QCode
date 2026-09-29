package tool

import (
	"reflect"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

func TestResourceSecurityMapping(t *testing.T) {
	tests := []struct {
		name   string
		source Resource
		want   securitymodel.Resource
		ok     bool
	}{
		{
			name:   "file",
			source: Resource{Kind: "file", Path: "src/a.go", Access: AccessWrite},
			want:   securitymodel.Resource{Class: securitymodel.ClassPath, Path: "src/a.go", Access: AccessWrite},
			ok:     true,
		},
		{
			name:   "repository tree by id",
			source: Resource{Kind: "repo", ID: ".", Access: AccessRead, Tree: true},
			want:   securitymodel.Resource{Class: securitymodel.ClassPath, Path: ".", Tree: true, Access: AccessRead},
			ok:     true,
		},
		{
			name:   "directory is a tree",
			source: Resource{Kind: "directory", Path: "build", Access: AccessWrite},
			want:   securitymodel.Resource{Class: securitymodel.ClassPath, Path: "build", Tree: true, Access: AccessWrite},
			ok:     true,
		},
		{
			name: "declared host endpoint",
			source: Resource{
				Kind: "host", ID: "example.com", Access: AccessWrite,
				Protocol: "http", Port: 80, Methods: []string{"GET"},
			},
			want: securitymodel.Resource{
				Class: securitymodel.ClassNetwork, Access: AccessWrite, Methods: []string{"GET"},
				Network: &netpolicy.Target{Scheme: "http", Host: "example.com", Port: 80},
			},
			ok: true,
		},
		{
			name:   "bare host defaults like a target",
			source: Resource{Kind: "host", ID: "example.com", Access: AccessRead},
			want: securitymodel.Resource{
				Class: securitymodel.ClassNetwork, Access: AccessRead,
				Network: &netpolicy.Target{Scheme: "https", Host: "example.com", Port: 443},
			},
			ok: true,
		},
		{
			name:   "url",
			source: Resource{Kind: "url", ID: "http://10.0.0.1:8080/x", Access: AccessRead, AllowPrivate: true},
			want: securitymodel.Resource{
				Class: securitymodel.ClassNetwork, Access: AccessRead, AllowPrivate: true,
				URL:     "http://10.0.0.1:8080/x",
				Network: &netpolicy.Target{Scheme: "http", Host: "10.0.0.1", Port: 8080},
			},
			ok: true,
		},
		{
			name:   "unparsable url stays unconstrained",
			source: Resource{Kind: "url", ID: "not a target", Access: AccessRead},
			want: securitymodel.Resource{
				Class: securitymodel.ClassNetwork, ID: "not a target", URL: "not a target", Access: AccessRead,
			},
			ok: true,
		},
		{
			name: "loopback grant",
			source: Resource{
				Kind: "host", ID: securitymodel.LoopbackHost, Access: AccessWrite,
				Protocol: securitymodel.LoopbackProtocol, Methods: []string{"BIND", "CONNECT"},
				AllowPrivate: true,
			},
			want: securitymodel.Resource{Class: securitymodel.ClassLoopback, Access: AccessWrite, Methods: []string{"BIND", "CONNECT"}, AllowPrivate: true},
			ok:   true,
		},
		{
			name:   "process",
			source: Resource{Kind: "process", ID: "workspace", Access: AccessRead, Tree: true},
			want:   securitymodel.Resource{Class: securitymodel.ClassProcess, ID: "workspace", Tree: true, Access: AccessRead},
			ok:     true,
		},
		{
			name:   "agent",
			source: Resource{Kind: "agent", ID: "agent-1", Access: AccessWrite},
			want:   securitymodel.Resource{Class: securitymodel.ClassAgent, ID: "agent-1", Access: AccessWrite},
			ok:     true,
		},
		{
			name:   "plan",
			source: Resource{Kind: "plan", ID: "session", Access: AccessWrite},
			want:   securitymodel.Resource{Class: securitymodel.ClassPlan, ID: "session", Access: AccessWrite},
			ok:     true,
		},
		{
			name:   "session",
			source: Resource{Kind: "session", ID: "term-1", Access: AccessWrite},
			want:   securitymodel.Resource{Class: securitymodel.ClassSession, ID: "term-1", Access: AccessWrite},
			ok:     true,
		},
		{
			name:   "named",
			source: Resource{Kind: "vcs_branch", ID: "main", Access: AccessWrite},
			want: securitymodel.Resource{
				Class: securitymodel.ClassNamed, Name: "vcs_branch", ID: "main", Access: AccessWrite,
			},
			ok: true,
		},
		{
			name:   "scheduling lock is not a security resource",
			source: Resource{Kind: "parallel", ID: "serial-tools", Access: AccessWrite, Tree: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := test.source.Security()
			if ok != test.ok || (ok && !reflect.DeepEqual(got, test.want)) {
				t.Fatalf("Security() = %+v, %v; want %+v, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestEffectContractDeclarationsValidate(t *testing.T) {
	fixed := EffectContract{
		Mode: EffectFixed, Kind: EffectAgentLifecycle, Risk: RiskMedium,
		Reversibility: Bounded, WorkspaceTransaction: TransactionNone,
		Approval: ApprovalPolicyDefault,
	}
	valid := fixed
	valid.ReadOnlyWhen = &ArgumentMatch{Field: "role", Values: []string{"review"}}
	valid.Planning = PlanningExempt
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*EffectContract){
		"derived read-only": func(e *EffectContract) {
			*e = EffectContract{
				Mode: EffectDerived, WorkspaceTransaction: TransactionNone,
				Approval:     ApprovalPolicyDefault,
				ReadOnlyWhen: &ArgumentMatch{Field: "role", Values: []string{"review"}},
			}
		},
		"empty field":  func(e *EffectContract) { e.ReadOnlyWhen = &ArgumentMatch{Values: []string{"review"}} },
		"empty values": func(e *EffectContract) { e.ReadOnlyWhen = &ArgumentMatch{Field: "role"} },
		"planning":     func(e *EffectContract) { e.Planning = "sometimes" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := fixed
			mutate(&invalid)
			if invalid.Validate() == nil {
				t.Fatal("invalid declaration passed validation")
			}
		})
	}
	match := ArgumentMatch{Field: "role", Values: []string{"review", "explore"}}
	if !match.Matches(" Review ") || match.Matches("reviewer") || match.Matches("") {
		t.Fatal("argument match normalization is wrong")
	}
}

func TestVerificationFieldRequiresEvidenceAndCoverage(t *testing.T) {
	binding := TrustedBinding{
		Capability: CapabilityProcess, AccessMode: AccessRead,
		ParallelPolicy: ParallelConcurrent, SandboxRequirement: SandboxNone,
		ResourceResolver: ResourceResolver{ReadPathsField: "covered_paths"},
		Effect: EffectContract{
			Mode: EffectDerived, WorkspaceTransaction: TransactionNone,
			Approval: ApprovalPolicyDefault,
		},
		ProducesVerificationEvidence: true,
		VerificationField:            "verification",
	}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	noEvidence := binding
	noEvidence.ProducesVerificationEvidence = false
	if noEvidence.Validate() == nil {
		t.Fatal("verification field without evidence passed validation")
	}
	noCoverage := binding
	noCoverage.ResourceResolver.ReadPathsField = ""
	if noCoverage.Validate() == nil {
		t.Fatal("verification field without covered paths passed validation")
	}
}
