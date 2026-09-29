package environment

const (
	CategoryEnvironmentResourceUnavailable = "environment_resource_unavailable"
	CategoryFilesystemAccessDenied         = "filesystem_access_denied"
	CategoryNetworkTargetUnapproved        = "network_target_unapproved"
	CategoryCredentialUnavailable          = "credential_unavailable"
	CategoryCredentialRejected             = "credential_rejected"
	CategoryTrustValidationFailed          = "trust_validation_failed"
	CategoryUpstreamUnavailable            = "upstream_unavailable"
	CategoryBackendCapabilityUnsupported   = "backend_capability_unsupported"
	CategorySourceUnbound                  = "source_unbound"
	CategoryUnknown                        = "unknown"
)

const (
	ActionApproveHostConfig     = "approve_host_config"
	ActionBindCredential        = "bind_credential"
	ActionEnableSharedUserTemp  = "enable_shared_user_temp"
	ActionApproveNetworkTarget  = "approve_network_target"
	ActionBindEnvironmentSource = "bind_environment_source"
)

const (
	SourceUserDeclaration = "user-declaration"
	SourcePreparer        = "preparer"
	SourceGate            = "gate"
	SourceProcessProxy    = "process_proxy"
	SourceOSBackend       = "os_backend"
	SourceProcess         = "process"
	SourceAuthService     = "auth_service"
)

// Fact is a typed missing-capability or environment failure produced by an
// authoritative component. Callers must never infer Category from process
// output text such as "401" or "permission denied".
type Fact struct {
	Source             string `json:"source"`
	Category           string `json:"error_category"`
	RequiredAction     string `json:"required_action,omitempty"`
	Resource           string `json:"resource,omitempty"`
	EnvironmentVersion string `json:"environment_version,omitempty"`
	AuthorityVersion   string `json:"authority_version,omitempty"`
	HasSideEffects     bool   `json:"has_side_effects,omitempty"`
	Detail             string `json:"detail,omitempty"`
}

// ClassifyFromAuthority returns the first typed category. Empty input and
// facts that only carry unknown are reported as unknown. Detail is evidence
// text, never a classification signal.
func ClassifyFromAuthority(facts []Fact) (category, action string) {
	for _, fact := range facts {
		if fact.Category == "" || fact.Category == CategoryUnknown {
			continue
		}
		return fact.Category, fact.RequiredAction
	}
	return CategoryUnknown, ""
}
