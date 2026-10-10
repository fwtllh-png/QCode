package protocol

import (
	"errors"
	"strings"
	"time"
)

// ToolExecutionReceipt is the durable projection of one guarded tool execution.
type ToolExecutionReceipt struct {
	GuardianReviewID string               `json:"guardian_review_id,omitempty"`
	Tool             ToolExecutionRef     `json:"tool"`
	Source           string               `json:"source"`
	Disposition      string               `json:"disposition"`
	PolicyDenial     *ToolPolicyDecision  `json:"policy_denial,omitempty"`
	Attempts         []ToolAttemptReceipt `json:"attempts"`
	ApprovalWait     time.Duration        `json:"approval_wait,omitempty"`
	DispatchWait     time.Duration        `json:"dispatch_wait,omitempty"`
	ClaimWait        time.Duration        `json:"claim_wait,omitempty"`
	TerminalStatus   string               `json:"terminal_status"`
	TerminalOwner    string               `json:"terminal_owner"`
	Teardown         time.Duration        `json:"teardown,omitempty"`
	TeardownMS       int64                `json:"teardown_ms,omitempty"`
	TeardownTimedOut bool                 `json:"teardown_timed_out,omitempty"`
}

type ToolExecutionRef struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	CatalogID  string `json:"catalog_id"`
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
}

// ToolPolicyDecision names the policy decision layer that decided a call.
type ToolPolicyDecision struct {
	Action string `json:"action"`
	Layer  string `json:"layer"`
	Code   string `json:"code,omitempty"`
}

type ToolPermissionProvenance struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Digest   string `json:"digest,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

type ToolPermissionAmendmentReceipt struct {
	BasePermissionDigest    string `json:"base_permission_digest"`
	Kind                    string `json:"kind"`
	Resource                string `json:"resource"`
	Protocol                string `json:"protocol,omitempty"`
	Port                    uint16 `json:"port,omitempty"`
	Capability              string `json:"capability,omitempty"`
	Decision                string `json:"decision"`
	AmendedPermissionDigest string `json:"amended_permission_digest,omitempty"`
}

type ToolSandboxDenial struct {
	Backend    string `json:"backend"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	ReasonCode string `json:"reason_code"`
	Protocol   string `json:"protocol,omitempty"`
	Port       uint16 `json:"port,omitempty"`
}

type ToolAttemptReceipt struct {
	Sequence                uint32                          `json:"sequence"`
	Sandbox                 string                          `json:"sandbox"`
	Status                  string                          `json:"status"`
	TerminalOwner           string                          `json:"terminal_owner"`
	Reason                  string                          `json:"reason,omitempty"`
	OperationSchemaVersion  int                             `json:"operation_schema_version,omitempty"`
	OperationDigest         string                          `json:"operation_digest,omitempty"`
	LeaseID                 string                          `json:"lease_id,omitempty"`
	LeaseState              string                          `json:"lease_state,omitempty"`
	LeaseAttempt            uint64                          `json:"lease_attempt,omitempty"`
	WorkspaceID             string                          `json:"workspace_id,omitempty"`
	WorkspaceGeneration     uint64                          `json:"workspace_generation,omitempty"`
	SubjectKind             string                          `json:"subject_kind,omitempty"`
	SubjectID               string                          `json:"subject_id,omitempty"`
	SubjectDigest           string                          `json:"subject_digest,omitempty"`
	SubjectGeneration       uint64                          `json:"subject_generation,omitempty"`
	PolicyRevision          uint64                          `json:"policy_revision,omitempty"`
	Policy                  *ToolPolicyDecision             `json:"policy,omitempty"`
	SandboxPolicyID         string                          `json:"sandbox_policy_id,omitempty"`
	EffectKind              string                          `json:"effect_kind,omitempty"`
	EffectRisk              string                          `json:"effect_risk,omitempty"`
	EffectReversibility     string                          `json:"effect_reversibility,omitempty"`
	WorkspaceTransaction    string                          `json:"workspace_transaction,omitempty"`
	PermissionSchemaVersion int                             `json:"permission_schema_version,omitempty"`
	PermissionRevision      uint64                          `json:"permission_revision,omitempty"`
	PermissionDigest        string                          `json:"permission_digest,omitempty"`
	PermissionCapability    string                          `json:"permission_capability,omitempty"`
	PermissionAccess        string                          `json:"permission_access,omitempty"`
	Enforcement             string                          `json:"enforcement,omitempty"`
	Backend                 string                          `json:"backend,omitempty"`
	EffectiveControls       map[string]string               `json:"effective_controls"`
	WorkspaceRoot           string                          `json:"workspace_root,omitempty"`
	ReadRoots               []string                        `json:"read_roots,omitempty"`
	WritePaths              []string                        `json:"write_paths,omitempty"`
	DeniedWriteRoots        []string                        `json:"denied_write_roots,omitempty"`
	WorkspaceBaseWrite      bool                            `json:"workspace_base_write,omitempty"`
	FullAccess              bool                            `json:"full_access,omitempty"`
	NetworkMode             string                          `json:"network_mode,omitempty"`
	NetworkTargets          []string                        `json:"network_targets,omitempty"`
	ManagedProxyPort        uint16                          `json:"managed_proxy_port,omitempty"`
	LoopbackAllowed         bool                            `json:"loopback_allowed,omitempty"`
	ProcessAllowed          bool                            `json:"process_allowed,omitempty"`
	Provenance              []ToolPermissionProvenance      `json:"provenance,omitempty"`
	Denial                  *ToolSandboxDenial              `json:"denial,omitempty"`
	Amendment               *ToolPermissionAmendmentReceipt `json:"amendment,omitempty"`
	StartedAt               time.Time                       `json:"started_at"`
	CompletedAt             time.Time                       `json:"completed_at"`
	DurationMS              int64                           `json:"duration_ms"`
	Teardown                time.Duration                   `json:"teardown,omitempty"`
	TeardownMS              int64                           `json:"teardown_ms,omitempty"`
	TeardownTimedOut        bool                            `json:"teardown_timed_out,omitempty"`
}

func (r *ToolExecutionReceipt) validate() error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.Tool.Name) == "" ||
		strings.TrimSpace(r.Tool.Source) == "" ||
		strings.TrimSpace(r.Tool.CatalogID) == "" ||
		r.Tool.Generation == 0 ||
		r.Tool.Revision == 0 {
		return errors.New("tool execution receipt identity is incomplete")
	}
	if r.Source == "" || r.Disposition == "" || r.TerminalStatus == "" || r.TerminalOwner == "" {
		return errors.New("tool execution receipt terminal evidence is incomplete")
	}
	if err := r.PolicyDenial.validate(); err != nil {
		return err
	}
	for _, attempt := range r.Attempts {
		if err := attempt.Policy.validate(); err != nil {
			return err
		}
		if attempt.Sequence == 0 || attempt.Sandbox == "" ||
			attempt.Status == "" || attempt.TerminalOwner == "" ||
			attempt.StartedAt.IsZero() || attempt.CompletedAt.IsZero() {
			return errors.New("tool execution attempt receipt is incomplete")
		}
		if attempt.PermissionDigest != "" &&
			len(attempt.PermissionDigest) != 64 {
			return errors.New("tool execution permission digest is invalid")
		}
		if attempt.OperationDigest != "" &&
			(len(attempt.OperationDigest) != 64 ||
				attempt.OperationSchemaVersion == 0 ||
				attempt.LeaseID == "" ||
				attempt.LeaseState == "" ||
				attempt.LeaseAttempt == 0) {
			return errors.New("tool execution lease evidence is invalid")
		}
	}
	return nil
}

func (d *ToolPolicyDecision) validate() error {
	if d == nil {
		return nil
	}
	if d.Action == "" || d.Layer == "" {
		return errors.New("tool execution policy decision is incomplete")
	}
	return nil
}
