package authority

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type LeaseState string

const (
	LeaseIssued   LeaseState = "issued"
	LeaseConsumed LeaseState = "consumed"
	LeaseSettled  LeaseState = "settled"
	LeaseExpired  LeaseState = "expired"
	LeaseRevoked  LeaseState = "revoked"
)

type ExecutionLease struct {
	id                  string
	nonce               string
	operationDigest     string
	workspaceID         string
	workspaceGeneration uint64
	subjectDigest       string
	subjectGeneration   uint64
	policyRevision      uint64
	sandboxPolicyID     string
	resourceBindings    []ResourceBinding
	artifactDigest      string
	profile             EffectivePermissionProfile
	attempt             uint64
	expiresAt           time.Time
}

type LeaseSnapshot struct {
	ID                  string                     `json:"id"`
	State               LeaseState                 `json:"state"`
	OperationDigest     string                     `json:"operation_digest"`
	WorkspaceID         string                     `json:"workspace_id"`
	WorkspaceGeneration uint64                     `json:"workspace_generation"`
	SubjectDigest       string                     `json:"subject_digest"`
	SubjectGeneration   uint64                     `json:"subject_generation"`
	PolicyRevision      uint64                     `json:"policy_revision"`
	SandboxPolicyID     string                     `json:"sandbox_policy_id,omitempty"`
	ResourceBindings    []ResourceBinding          `json:"resource_bindings"`
	ArtifactDigest      string                     `json:"artifact_digest,omitempty"`
	PermissionProfile   EffectivePermissionProfile `json:"permission_profile"`
	Attempt             uint64                     `json:"attempt"`
	ExpiresAt           time.Time                  `json:"expires_at"`
}

type ResourceBinding struct {
	Namespace      ResourceNamespace `json:"namespace"`
	RootID         string            `json:"root_id,omitempty"`
	RootGeneration uint64            `json:"root_generation,omitempty"`
	ResourceDigest string            `json:"resource_digest"`
}

type LeaseIssueRequest struct {
	Operation       ExecutionOperation
	Profile         EffectivePermissionProfile
	PolicyRevision  uint64
	SandboxPolicyID string
	Attempt         uint64
	ExpiresAt       time.Time
}

type LeaseValidation struct {
	Operation           ExecutionOperation
	PolicyRevision      uint64
	WorkspaceID         string
	WorkspaceGeneration uint64
	SubjectDigest       string
	SubjectGeneration   uint64
	SandboxPolicyID     string
	ArtifactDigest      string
	Attempt             uint64
}

type Settlement struct {
	Status      string
	Reason      string
	CompletedAt time.Time
}

type leaseRecord struct {
	started    bool
	lease      ExecutionLease
	state      LeaseState
	settlement Settlement
}

type LeaseAuthorityOptions struct {
	Now    func() time.Time
	Random io.Reader
}

type LeaseAuthority struct {
	mu      sync.Mutex
	now     func() time.Time
	random  io.Reader
	leases  map[string]*leaseRecord
	handles map[string]*processHandleRecord
}

func NewLeaseAuthority(options LeaseAuthorityOptions) *LeaseAuthority {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &LeaseAuthority{
		now: options.Now, random: options.Random,
		leases:  make(map[string]*leaseRecord),
		handles: make(map[string]*processHandleRecord),
	}
}

func (a *LeaseAuthority) Issue(request LeaseIssueRequest) (ExecutionLease, error) {
	if a == nil {
		return ExecutionLease{}, errors.New("lease authority is required")
	}
	if err := request.Operation.Validate(); err != nil {
		return ExecutionLease{}, err
	}
	if err := request.Profile.Validate(); err != nil {
		return ExecutionLease{}, err
	}
	if request.PolicyRevision == 0 ||
		request.Attempt == 0 ||
		request.ExpiresAt.IsZero() ||
		!request.ExpiresAt.After(a.now()) {
		return ExecutionLease{}, errors.New("lease issue request is incomplete")
	}
	if request.Profile.Revision != request.Attempt {
		return ExecutionLease{}, errors.New("lease attempt does not match permission revision")
	}
	if request.Profile.Tool != request.Operation.Tool {
		return ExecutionLease{}, errors.New("lease operation does not match permission tool")
	}
	if request.Profile.Process.Enforcement == sandbox.EnforcementStrong &&
		request.SandboxPolicyID == "" {
		return ExecutionLease{}, errors.New("strong execution lease requires a sandbox policy")
	}
	if err := request.Operation.Required.SatisfiedBy(
		effectiveControls(request.Profile, request.Operation),
	); err != nil {
		return ExecutionLease{}, err
	}
	bindings, err := resourceBindings(request.Operation.Resources)
	if err != nil {
		return ExecutionLease{}, err
	}
	id, err := randomToken(a.random)
	if err != nil {
		return ExecutionLease{}, err
	}
	nonce, err := randomToken(a.random)
	if err != nil {
		return ExecutionLease{}, err
	}
	artifactDigest := ""
	if request.Operation.Artifact != nil {
		artifactDigest = request.Operation.Artifact.ManifestDigest
	}
	lease := ExecutionLease{
		id: id, nonce: nonce,
		operationDigest:     request.Operation.Digest,
		workspaceID:         request.Operation.WorkspaceID,
		workspaceGeneration: request.Operation.WorkspaceGeneration,
		subjectDigest:       request.Operation.Subject.Digest,
		subjectGeneration:   request.Operation.Subject.Generation,
		policyRevision:      request.PolicyRevision,
		sandboxPolicyID:     request.SandboxPolicyID,
		resourceBindings:    bindings,
		artifactDigest:      artifactDigest,
		profile:             cloneProfile(request.Profile),
		attempt:             request.Attempt, expiresAt: request.ExpiresAt,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leases[id] != nil {
		return ExecutionLease{}, errors.New("execution lease identity collision")
	}
	a.leases[id] = &leaseRecord{lease: lease, state: LeaseIssued}
	return cloneLease(lease), nil
}

func (a *LeaseAuthority) Consume(
	lease ExecutionLease,
	current LeaseValidation,
) error {
	if a == nil {
		return errors.New("lease authority is required")
	}
	if err := current.Operation.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return err
	}
	if record.state != LeaseIssued {
		return errors.New("execution lease is not issuable")
	}
	if !record.lease.expiresAt.After(a.now()) {
		record.state = LeaseExpired
		return errors.New("execution lease expired")
	}
	if err := validateLeaseCurrent(record.lease, current); err != nil {
		return err
	}
	record.state = LeaseConsumed
	return nil
}

func (a *LeaseAuthority) Settle(
	lease ExecutionLease,
	settlement Settlement,
) error {
	if a == nil {
		return errors.New("lease authority is required")
	}
	if settlement.Status == "" || settlement.CompletedAt.IsZero() {
		return errors.New("lease settlement is incomplete")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return err
	}
	if record.state == LeaseSettled {
		if record.settlement == settlement {
			return nil
		}
		return errors.New("execution lease was settled with another result")
	}
	if record.state != LeaseConsumed {
		return errors.New("only a consumed execution lease can settle")
	}
	record.state = LeaseSettled
	record.settlement = settlement
	return nil
}

func (a *LeaseAuthority) Revoke(lease ExecutionLease) error {
	if a == nil {
		return errors.New("lease authority is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return err
	}
	switch record.state {
	case LeaseIssued:
		record.state = LeaseRevoked
		return nil
	case LeaseRevoked:
		return nil
	default:
		return errors.New("execution lease can no longer be revoked")
	}
}

func (a *LeaseAuthority) Snapshot(lease ExecutionLease) (LeaseSnapshot, error) {
	if a == nil {
		return LeaseSnapshot{}, errors.New("lease authority is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return LeaseSnapshot{}, err
	}
	return snapshot(record.lease, record.state), nil
}

func (a *LeaseAuthority) Release(lease ExecutionLease) error {
	if a == nil {
		return errors.New("lease authority is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return err
	}
	switch record.state {
	case LeaseSettled, LeaseExpired, LeaseRevoked:
	default:
		return errors.New("execution lease is not terminal")
	}
	for id, handle := range a.handles {
		if handle.handle.leaseID != lease.id {
			continue
		}
		if !handle.terminal {
			return errors.New("execution lease has an active process handle")
		}
		delete(a.handles, id)
	}
	delete(a.leases, lease.id)
	return nil
}

func (a *LeaseAuthority) authenticRecord(
	lease ExecutionLease,
) (*leaseRecord, error) {
	if lease.id == "" || lease.nonce == "" {
		return nil, errors.New("execution lease is invalid")
	}
	record := a.leases[lease.id]
	if record == nil || record.lease.nonce != lease.nonce {
		return nil, errors.New("execution lease is not authentic")
	}
	return record, nil
}

func validateLeaseCurrent(lease ExecutionLease, current LeaseValidation) error {
	artifactDigest := ""
	if current.Operation.Artifact != nil {
		artifactDigest = current.Operation.Artifact.ManifestDigest
	}
	switch {
	case current.Operation.Digest != lease.operationDigest:
		return errors.New("execution lease operation changed")
	case current.WorkspaceID != lease.workspaceID:
		return errors.New("execution lease workspace changed")
	case current.WorkspaceGeneration != lease.workspaceGeneration:
		return errors.New("execution lease workspace generation changed")
	case current.SubjectDigest != lease.subjectDigest:
		return errors.New("execution lease subject changed")
	case current.SubjectGeneration != lease.subjectGeneration:
		return errors.New("execution lease subject generation changed")
	case current.PolicyRevision != lease.policyRevision:
		return errors.New("execution lease policy revision changed")
	case current.SandboxPolicyID != lease.sandboxPolicyID:
		return errors.New("execution lease sandbox policy changed")
	case current.ArtifactDigest != lease.artifactDigest ||
		artifactDigest != lease.artifactDigest:
		return errors.New("execution lease artifact changed")
	case current.Attempt != lease.attempt:
		return errors.New("execution lease attempt changed")
	default:
		return nil
	}
}

func effectiveControls(
	profile EffectivePermissionProfile,
	operation ExecutionOperation,
) securitymodel.Controls {
	controls := profile.Controls
	if operation.Artifact != nil {
		controls.ArtifactOrigin = securitymodel.ArtifactOriginBrokerSnapshot
	}
	if operation.Effect.WorkspaceTransaction == WorkspaceTransactionBeforeImage {
		controls.DurableRecovery = securitymodel.DurableRecoveryExternalJournal
	}
	return controls
}

func resourceBindings(resources []Resource) ([]ResourceBinding, error) {
	result := make([]ResourceBinding, 0, len(resources))
	for _, resource := range resources {
		digest, err := resourceDigest(resource)
		if err != nil {
			return nil, err
		}
		result = append(result, ResourceBinding{
			Namespace: resource.Namespace, RootID: resource.RootID,
			RootGeneration: resource.RootGeneration, ResourceDigest: digest,
		})
	}
	return result, nil
}

func snapshot(lease ExecutionLease, state LeaseState) LeaseSnapshot {
	return LeaseSnapshot{
		ID: lease.id, State: state, OperationDigest: lease.operationDigest,
		WorkspaceID:         lease.workspaceID,
		WorkspaceGeneration: lease.workspaceGeneration,
		SubjectDigest:       lease.subjectDigest,
		SubjectGeneration:   lease.subjectGeneration,
		PolicyRevision:      lease.policyRevision,
		SandboxPolicyID:     lease.sandboxPolicyID,
		ResourceBindings:    append([]ResourceBinding(nil), lease.resourceBindings...),
		ArtifactDigest:      lease.artifactDigest,
		PermissionProfile:   cloneProfile(lease.profile),
		Attempt:             lease.attempt, ExpiresAt: lease.expiresAt,
	}
}

func cloneLease(source ExecutionLease) ExecutionLease {
	cloned := source
	cloned.resourceBindings = append(
		[]ResourceBinding(nil),
		source.resourceBindings...,
	)
	cloned.profile = cloneProfile(source.profile)
	return cloned
}

func randomToken(source io.Reader) (string, error) {
	var value [32]byte
	if _, err := io.ReadFull(source, value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
