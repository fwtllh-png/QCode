package tool

import (
	"encoding/json"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestSecurityInvocationPreservesSnapshotAndBindsCatalogIdentity(t *testing.T) {
	for _, tc := range []struct {
		source string
		kind   securitymodel.SubjectKind
		trust  securitymodel.TrustLevel
	}{
		{"builtin:probe:1", securitymodel.SubjectBuiltin, securitymodel.TrustBuiltin},
		{"mcp:server", securitymodel.SubjectMCPTool, securitymodel.TrustExternal},
		{"external:extension", securitymodel.SubjectHost, securitymodel.TrustExternal},
		{"dynamic:1", securitymodel.SubjectHost, securitymodel.TrustHost},
	} {
		t.Run(tc.source, func(t *testing.T) {
			binding := TrustedBinding{Capability: CapabilityRead, AccessMode: AccessRead}
			prepared := PreparedInvocation{
				CallID: "call", Tool: "probe", Binding: binding,
				Ref:        ToolRef{Name: "probe", Source: tc.source, CatalogID: "catalog", Generation: 1, Revision: 2, Authority: 3},
				Arguments:  json.RawMessage(`{"path":"a"}`),
				Assessment: AssessResources(binding, securitymodel.Declared{}, []Resource{{Kind: "file", Path: "/a", Access: AccessRead}}),
			}
			input, err := prepared.SecurityInvocation()
			if err != nil {
				t.Fatal(err)
			}
			if !input.Assessment.Same(prepared.Assessment) || input.Subject.Kind != tc.kind || input.Subject.Trust != tc.trust {
				t.Fatalf("unexpected security projection: %+v", input)
			}
			if err := input.Subject.Validate(); err != nil {
				t.Fatal(err)
			}
			prepared.Arguments[0] = '['
			if string(input.Arguments) != `{"path":"a"}` {
				t.Fatal("arguments alias the adapter buffer")
			}
			prepared.Ref.Authority++
			changed, err := prepared.SecurityInvocation()
			if err != nil || changed.Subject.Digest == input.Subject.Digest {
				t.Fatal("catalog authority change did not change subject identity")
			}
			prepared.Ref.Name = "different"
			if _, err := prepared.SecurityInvocation(); err == nil {
				t.Fatal("mismatched catalog identity accepted")
			}
		})
	}
}
