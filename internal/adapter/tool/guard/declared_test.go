package guard

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestPreparationResolvesDeclaredFacts(t *testing.T) {
	spawn := tool.TrustedBinding{
		Capability: tool.CapabilityWrite,
		Effect: tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectAgentLifecycle,
			Risk: tool.RiskMedium, Reversibility: tool.Bounded,
			ReadOnlyWhen: &tool.ArgumentMatch{
				Field: "role", Values: []string{"review", "explore"},
			},
		},
	}
	verify := tool.TrustedBinding{
		Capability:                   tool.CapabilityProcess,
		ResourceResolver:             tool.ResourceResolver{ReadPathsField: "covered_paths"},
		ProducesVerificationEvidence: true,
		VerificationField:            "verification",
	}
	undeclared := verify
	undeclared.VerificationField = ""
	tests := []struct {
		name      string
		binding   tool.TrustedBinding
		arguments string
		want      securitymodel.Declared
	}{
		{"read-only role", spawn, `{"role":" Explore "}`, securitymodel.Declared{ReadOnly: true}},
		{"writing role", spawn, `{"role":"implementer"}`, securitymodel.Declared{}},
		{"role alias is not declared", spawn, `{"role":"reviewer"}`, securitymodel.Declared{}},
		{"non-string role", spawn, `{"role":["review"]}`, securitymodel.Declared{}},
		{
			"verification with coverage", verify,
			`{"command":"go test ./...","verification":"test","covered_paths":["a.go"]}`,
			securitymodel.Declared{Verification: true},
		},
		{"verification without coverage", verify, `{"verification":"test"}`, securitymodel.Declared{}},
		{"blank verification", verify, `{"verification":" ","covered_paths":["a.go"]}`, securitymodel.Declared{}},
		{
			"binding without declaration", undeclared,
			`{"verification":"test","covered_paths":["a.go"]}`, securitymodel.Declared{},
		},
		{"malformed arguments", verify, `[]`, securitymodel.Declared{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := declaredFacts(Invocation{
				Tool: "fixture", Arguments: json.RawMessage(test.arguments),
				Binding: test.binding,
			})
			if got != test.want {
				t.Fatalf("declared = %+v, want %+v", got, test.want)
			}
		})
	}
}
