package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// SecurityInvocation projects a registry-validated invocation into the
// security-owned input. It preserves the assessment and does not resolve or
// classify resources again.
func (p PreparedInvocation) SecurityInvocation() (securitymodel.PreparedInvocation, error) {
	if err := p.Ref.Validate(); err != nil {
		return securitymodel.PreparedInvocation{}, err
	}
	if p.Tool != p.Ref.Name {
		return securitymodel.PreparedInvocation{}, errors.New("invocation does not match the catalog tool reference")
	}
	kind, trust := securitymodel.SubjectBuiltin, securitymodel.TrustBuiltin
	switch CatalogSourceKind(p.Tool, p.Ref.Source) {
	case securitymodel.SourceMCP:
		kind, trust = securitymodel.SubjectMCPTool, securitymodel.TrustExternal
	case securitymodel.SourceExternal:
		kind, trust = securitymodel.SubjectHost, securitymodel.TrustExternal
	case securitymodel.SourceDynamic:
		kind, trust = securitymodel.SubjectHost, securitymodel.TrustHost
	}
	material := struct {
		Name, Source, CatalogID         string
		Generation, Revision, Authority uint64
	}{p.Ref.Name, p.Ref.Source, p.Ref.CatalogID, p.Ref.Generation, p.Ref.Revision, p.Ref.Authority}
	encoded, err := json.Marshal(material)
	if err != nil {
		return securitymodel.PreparedInvocation{}, err
	}
	sum := sha256.Sum256(encoded)
	return securitymodel.PreparedInvocation{
		CallID: p.CallID, Tool: p.Tool,
		Arguments:  append(json.RawMessage(nil), p.Arguments...),
		Assessment: p.Assessment, Required: p.Binding.Required,
		Subject: securitymodel.Subject{
			Kind: kind, Trust: trust, ID: CatalogToolID(p.Tool, p.Ref.Source),
			Digest: hex.EncodeToString(sum[:]), Generation: p.Ref.Generation,
		},
	}, nil
}
