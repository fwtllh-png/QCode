package authority

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// policyInvocationFixture keeps test declarations separate from the immutable assessment
// sent to policy. Mutating a fixture requires an explicit resolveAssessment call.
type policyInvocationFixture struct {
	CallID, Tool, Source string
	Arguments            json.RawMessage
	Resources            []tool.Resource
	Capability           tool.Capability
	Access               tool.AccessMode
	Sandbox              tool.SandboxRequirement
	Effect               tool.EffectContract
	Declared             securitymodel.Declared
	Journaled, Validated bool
	Workspace            string
	Stage                string
}

func (i policyInvocationFixture) resolveAssessment() securitymodel.Assessment {
	contract := i.Effect
	if i.Journaled {
		contract.WorkspaceTransaction = tool.TransactionBeforeImage
	} else {
		contract.WorkspaceTransaction = tool.TransactionNone
	}
	return tool.AssessResources(tool.TrustedBinding{
		Capability: i.Capability, AccessMode: i.Access, SandboxRequirement: i.Sandbox,
		Effect: contract,
	}, i.Declared, i.Resources)
}

func resolvePolicyFixture(i policyInvocationFixture) policy.Invocation {
	return policy.Invocation{CallID: i.CallID, Tool: i.Tool, Source: tool.CatalogSourceKind(i.Tool, i.Source), Arguments: i.Arguments,
		Assessment: i.resolveAssessment(), Approval: i.Effect.Approval, Validated: i.Validated,
		Workspace: i.Workspace, Stage: policy.Stage(i.Stage)}
}

func appendFixtureResources(prepared *securitymodel.PreparedInvocation, resources ...tool.Resource) {
	resolved := prepared.Assessment.Input()
	for _, item := range resources {
		if typed, ok := item.Security(); ok {
			resolved.Resources = append(resolved.Resources, typed)
		}
	}
	prepared.Assessment = securitymodel.Assess(resolved)
}

// resolvePreparedFixture prepares declarations at the same boundary as Guard.
func resolvePreparedFixture(prepared tool.PreparedInvocation) securitymodel.PreparedInvocation {
	if !prepared.Assessment.Valid() {
		prepared.Assessment = tool.AssessResources(prepared.Binding, securitymodel.Declared{}, prepared.Resources)
	}
	resolved, err := prepared.SecurityInvocation()
	if err != nil {
		panic(err)
	}
	return resolved
}
