package guardian

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// InvocationIdentity distinguishes attempts, including parent authority on child
// calls. IDs come from Runtime, never model arguments.
type InvocationIdentity struct {
	ReviewID, WorkspaceID, SessionID, ThreadID, TurnID, CallID, AttemptID string
	WorkspaceGeneration                                                   uint64
}

type BindingIdentity struct {
	Tool, CatalogID, ArgumentsDigest       string
	CatalogGeneration, Revision, Authority uint64
	Subject                                securitymodel.Subject
}

// AuthorizationSource is an original user fact, not an assistant summary. Text
// is held by Runtime; the security contract binds its origin and exact bytes.
type AuthorizationSource struct {
	ID, ThreadID, TurnID, Role, Digest string
	Version                            uint64
	Revoked                            bool
}

type AuthorizationSnapshot struct {
	WorkspaceID, SessionID, ThreadID, Revision string
	ParentDigest                               string
	Sources                                    []AuthorizationSource
	Complete                                   bool
}

func (a AuthorizationSnapshot) Validate() error {
	if a.WorkspaceID == "" || a.SessionID == "" || a.ThreadID == "" ||
		!validDigest(a.Revision) || !a.Complete || len(a.Sources) == 0 ||
		(a.ParentDigest != "" && !validDigest(a.ParentDigest)) {
		return errors.New("guardian authorization snapshot is incomplete")
	}
	seen := make(map[string]bool)
	for _, source := range a.Sources {
		if source.ID == "" || source.ThreadID == "" || source.TurnID == "" ||
			source.Role != "user" || source.Version == 0 || !validDigest(source.Digest) || seen[source.ID] {
			return errors.New("guardian authorization source is invalid")
		}
		if source.ThreadID != a.ThreadID && a.ParentDigest == "" {
			return errors.New("guardian inherited source has no parent authority")
		}
		seen[source.ID] = true
	}
	return nil
}

func (a AuthorizationSnapshot) SourceIDs() map[string]bool {
	result := make(map[string]bool)
	if a.Validate() != nil {
		return result
	}
	for _, source := range a.Sources {
		result[source.ID] = !source.Revoked
	}
	return result
}

func (a AuthorizationSnapshot) Digest() string { return digestValue(a) }

type ContentEvidence struct {
	Path, Identity, Digest string
	Mode                   uint32
	Size                   int64
}

// ExecutionSnapshot names the private copy retained for execution. Coverage
// describes whether all risk-relevant dependencies are present; a list of
// hashes alone does not prove that scripts have no dynamic dependencies.
type ExecutionSnapshot struct {
	ID, Root, RootIdentity, WorkingDir, WorkingDirIdentity    string
	Command, EnvironmentDigest, ResourcesDigest, AssessmentID string
	SandboxPolicyID, Settlement                               string
	Controls                                                  securitymodel.RequiredControls
	Content                                                   []ContentEvidence
	WritePaths                                                []string
	CoverageComplete                                          bool
	Missing                                                   []string
}

func (s ExecutionSnapshot) Validate() error {
	if s.ID == "" || !filepath.IsAbs(s.Root) || s.RootIdentity == "" ||
		!filepath.IsAbs(s.WorkingDir) || s.WorkingDirIdentity == "" || strings.TrimSpace(s.Command) == "" ||
		!validDigest(s.EnvironmentDigest) || !validDigest(s.ResourcesDigest) || !validDigest(s.AssessmentID) ||
		s.SandboxPolicyID == "" || (s.Settlement != "apply" && s.Settlement != "discard") ||
		!s.CoverageComplete || len(s.Missing) != 0 || s.Controls.IsZero() || s.Controls.Validate() != nil {
		return errors.New("guardian execution evidence is incomplete")
	}
	if rel, err := filepath.Rel(s.Root, s.WorkingDir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("guardian cwd is outside the execution snapshot")
	}
	seen := make(map[string]bool)
	for _, path := range s.WritePaths {
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || seen[path] {
			return errors.New("guardian write scope is invalid")
		}
		seen[path] = true
	}
	seen = make(map[string]bool)
	for _, content := range s.Content {
		if content.Path == "" || filepath.IsAbs(content.Path) || filepath.Clean(content.Path) != content.Path ||
			content.Path == ".." || strings.HasPrefix(content.Path, ".."+string(filepath.Separator)) ||
			content.Identity == "" || !validDigest(content.Digest) || content.Size < 0 || seen[content.Path] {
			return errors.New("guardian content identity is invalid")
		}
		seen[content.Path] = true
	}
	return nil
}

type ReviewVersions struct {
	PolicyRevision                                                             uint64
	Permission, ConfigurationDigest, RouteDigest, PromptVersion, SchemaVersion string
}

// ReviewCandidate contains only structured, owned values. Canonical JSON
// encoding keeps field boundaries and identities distinct in the digest.
// Construction is reserved for trusted Runtime/Guard integration, not tool JSON.
type ReviewCandidate struct {
	Identity      InvocationIdentity
	Binding       BindingIdentity
	Facts         CandidateFacts
	Execution     ExecutionSnapshot
	Authorization AuthorizationSnapshot
	Versions      ReviewVersions
}

func (c ReviewCandidate) Digest() string {
	return digestValue(struct {
		Version   int
		Candidate ReviewCandidate
	}{1, c})
}

func (c ReviewCandidate) Validate() error {
	i, b, v := c.Identity, c.Binding, c.Versions
	for _, id := range []string{i.ReviewID, i.WorkspaceID, i.SessionID, i.ThreadID, i.TurnID, i.CallID, i.AttemptID, b.Tool, b.CatalogID, v.PromptVersion, v.SchemaVersion} {
		if strings.TrimSpace(id) == "" {
			return errors.New("guardian candidate identity is incomplete")
		}
	}
	if i.WorkspaceGeneration == 0 || b.CatalogGeneration == 0 || b.Revision == 0 || b.Authority == 0 ||
		b.Subject.Validate() != nil || b.Subject.Kind != securitymodel.SubjectBuiltin || b.Subject.Trust != securitymodel.TrustBuiltin ||
		!validDigest(b.ArgumentsDigest) || v.PolicyRevision == 0 || v.Permission != "auto" ||
		!validDigest(v.ConfigurationDigest) || !validDigest(v.RouteDigest) {
		return errors.New("guardian binding or review version is incomplete")
	}
	if c.Authorization.WorkspaceID != i.WorkspaceID || c.Authorization.SessionID != i.SessionID || c.Authorization.ThreadID != i.ThreadID {
		return errors.New("guardian authorization belongs to another invocation")
	}
	if err := c.Authorization.Validate(); err != nil {
		return err
	}
	if err := c.Execution.Validate(); err != nil {
		return err
	}
	if result := CheckEligibility(c.Facts); !result.Eligible {
		return fmt.Errorf("guardian candidate: %s", result.Reason)
	}
	return nil
}

// ReviewEvidence is deliberately opaque: a raw assessment cannot bypass source
// validation, candidate eligibility or binding. It conveys no execution lease.
type ReviewEvidence struct {
	candidateDigest string
	assessment      Assessment
}

func BindAssessment(candidate ReviewCandidate, data []byte) (*ReviewEvidence, error) {
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	assessment, err := ParseAssessment(data, candidate.Authorization.SourceIDs())
	if err != nil {
		return nil, err
	}
	return &ReviewEvidence{candidateDigest: candidate.Digest(), assessment: assessment}, nil
}

func (e *ReviewEvidence) Assessment() Assessment {
	if e == nil {
		return Assessment{}
	}
	a := e.assessment
	a.AuthorizationSourceIDs = append([]string(nil), a.AuthorizationSourceIDs...)
	return a
}

func (e *ReviewEvidence) CandidateDigest() string {
	if e == nil {
		return ""
	}
	return e.candidateDigest
}

func digestValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
