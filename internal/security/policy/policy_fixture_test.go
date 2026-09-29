package policy

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// invocationFixture keeps test declarations separate from the immutable assessment
// sent to policy. Mutating a fixture requires an explicit resolveAssessment call.
type invocationFixture struct {
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

func (i invocationFixture) resolveAssessment() securitymodel.Assessment {
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

func resolveFixture(i invocationFixture) Invocation {
	return Invocation{CallID: i.CallID, Tool: i.Tool, Source: tool.CatalogSourceKind(i.Tool, i.Source), Arguments: i.Arguments,
		Assessment: i.resolveAssessment(), Approval: i.Effect.Approval, Validated: i.Validated,
		Workspace: i.Workspace, Stage: Stage(i.Stage)}
}
func assessFixture(i invocationFixture) securitymodel.Assessment { return i.resolveAssessment() }
