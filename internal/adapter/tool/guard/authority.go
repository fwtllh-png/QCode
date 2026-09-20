package guard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type LeaseAuthority = authority.LeaseAuthority

type AuthorizedProcessExecutor interface {
	tool.Executor
	PrepareAuthorizedProcess(
		context.Context,
		tool.PreparedInvocation,
		string,
	) (authority.ArtifactBinding, error)
	ReleaseAuthorizedProcess(context.Context, authority.ArtifactBinding) error
	ExecuteAuthorizedProcess(
		context.Context,
		tool.PreparedInvocation,
		authority.AuthorizedProcessGrant,
	) (tool.Result, tool.Outcome, error)
}

type AuthorizedFileExecutor interface {
	tool.Executor
	IsAuthorizedFileMutation(tool.PreparedInvocation) bool
	PrepareAuthorizedFile(
		context.Context,
		tool.PreparedInvocation,
	) (authority.FileBinding, error)
	ExecuteAuthorizedFile(
		context.Context,
		tool.PreparedInvocation,
		authority.AuthorizedFileGrant,
		*authority.LeaseAuthority,
		*workspacejournal.Manager,
	) (tool.Result, tool.Outcome, error)
}

func NewLeaseAuthority() *LeaseAuthority {
	return authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
}

func (g *Guard) compileAuthority(
	prepared preparedExecution,
	mode SandboxMode,
	revision uint64,
) (authority.EffectivePermissionProfile, error) {
	if prepared.runtime == nil {
		return authority.EffectivePermissionProfile{}, errors.New(
			"authorized policy snapshot is required",
		)
	}
	backend := g.registry.InjectedSandbox(prepared.invocation.Tool)
	var capability sandbox.Capability
	var sandboxPolicy sandbox.Policy
	if backend != nil {
		capability = backend.Capability()
		sandboxPolicy, _ = sandbox.BackendPolicy(backend)
	}
	if sandboxPolicy.WorkspaceRoot == "" {
		sandboxPolicy.WorkspaceRoot = g.workspace
	}
	enforcement := string(mode)
	profile, err := authority.Compile(authority.CompileInput{
		Runtime:       prepared.runtime,
		Invocation:    policyInput(prepared.invocation.CallID, prepared.invocation),
		Decision:      prepared.decision,
		Authorized:    true,
		Revision:      revision,
		Enforcement:   enforcement,
		Capability:    capability,
		SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		return authority.EffectivePermissionProfile{}, err
	}
	return profile, nil
}

func (g *Guard) issueExecutionLease(
	ctx context.Context,
	prepared preparedExecution,
	profile authority.EffectivePermissionProfile,
	attempt uint64,
	artifact *authority.ArtifactIntent,
	fileMutationDigest string,
	consume bool,
) (
	authority.ExecutionOperation,
	authority.ExecutionLease,
	authority.LeaseSnapshot,
	error,
) {
	operation, err := g.buildExecutionOperation(
		prepared, profile, artifact, fileMutationDigest,
	)
	if err != nil {
		return authority.ExecutionOperation{}, authority.ExecutionLease{},
			authority.LeaseSnapshot{}, err
	}
	sandboxPolicyID, err := sandboxPolicyBinding(profile, prepared.invocation.Tool)
	if err != nil {
		return operation, authority.ExecutionLease{},
			authority.LeaseSnapshot{}, err
	}
	expiresAt := g.now().Add(g.leaseTTL)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	lease, err := g.leaseAuthority.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision:  prepared.runtime.Revision,
		SandboxPolicyID: sandboxPolicyID, Attempt: attempt,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return operation, authority.ExecutionLease{},
			authority.LeaseSnapshot{}, err
	}
	validation := leaseValidation(
		operation, prepared.runtime.Revision, sandboxPolicyID, attempt,
	)
	if consume {
		err = g.leaseAuthority.Consume(lease, validation)
	}
	if err != nil {
		return operation, lease, authority.LeaseSnapshot{}, err
	}
	snapshot, err := g.leaseAuthority.Snapshot(lease)
	return operation, lease, snapshot, err
}

func (g *Guard) buildExecutionOperation(
	prepared preparedExecution,
	profile authority.EffectivePermissionProfile,
	artifact *authority.ArtifactIntent,
	fileMutationDigest string,
) (authority.ExecutionOperation, error) {
	policyInvocation := policyInput(
		prepared.invocation.CallID,
		prepared.invocation,
	)
	required := requiredControls(prepared.invocation)
	if artifact != nil {
		required.ArtifactOrigin = controlmatrix.ArtifactOriginBrokerSnapshot
	}
	operation, err := authority.BuildExecutionOperation(authority.OperationInput{
		WorkspaceRoot:          g.workspace,
		WorkspaceID:            g.workspaceID,
		WorkspaceGeneration:    g.workspaceGeneration,
		Invocation:             prepared.invocation,
		Effect:                 policy.NormalizeEffect(policyInvocation),
		Journaled:              policyInvocation.Journaled,
		RequireReadBeforeWrite: policyInvocation.Journaled,
		Required:               required,
		Artifact:               artifact,
		FileMutationDigest:     fileMutationDigest,
		HostReadRoots: append(
			append([]string(nil), profile.Filesystem.ReadRoots...),
			profile.Filesystem.WritePaths...,
		),
	})
	return operation, err
}

func sandboxPolicyBinding(
	profile authority.EffectivePermissionProfile,
	toolName string,
) (string, error) {
	sandboxPolicyID := ""
	for _, source := range profile.Provenance {
		if source.Kind == "sandbox" {
			sandboxPolicyID = source.Digest
			break
		}
	}
	if sandboxPolicyID == "" && profile.Process.Enforcement == "strong" {
		sandboxPolicyID = authority.FallbackSandboxPolicyID(
			profile.Filesystem.WorkspaceRoot,
			profile.Process.Backend,
			profile.Controls,
		)
		if sandboxPolicyID == "" {
			return "", fmt.Errorf(
				"derive sandbox policy binding for %q", toolName,
			)
		}
	}
	return sandboxPolicyID, nil
}

func leaseValidation(
	operation authority.ExecutionOperation,
	policyRevision uint64,
	sandboxPolicyID string,
	attempt uint64,
) authority.LeaseValidation {
	artifactDigest := ""
	if operation.Artifact != nil {
		artifactDigest = operation.Artifact.ManifestDigest
	}
	return authority.LeaseValidation{
		Operation: operation, PolicyRevision: policyRevision,
		WorkspaceID:         operation.WorkspaceID,
		WorkspaceGeneration: operation.WorkspaceGeneration,
		SubjectDigest:       operation.Subject.Digest,
		SubjectGeneration:   operation.Subject.Generation,
		SandboxPolicyID:     sandboxPolicyID, ArtifactDigest: artifactDigest,
		Attempt: attempt,
	}
}

func requiredControls(invocation Invocation) authority.RequiredControls {
	required := invocation.Binding.Required
	if invocation.Binding.SandboxRequirement == tool.SandboxStrong {
		var hasNetworkTargets, allowLoopback bool
		for _, resource := range invocation.Resources {
			if resource.Access == tool.AccessWrite &&
				isPathKind(resource.Kind) {
				required.FilesystemWrite = controlmatrix.FilesystemWriteExactPaths
				required.PathIdentity = controlmatrix.PathIdentityDescriptorRelative
			}
			if resource.Kind == "host" || resource.Kind == "url" {
				if resource.Kind == "host" && resource.Protocol == "loopback" {
					allowLoopback = true
				} else {
					hasNetworkTargets = true
				}
			}
		}
		switch {
		case hasNetworkTargets:
			required.Network = controlmatrix.NetworkProxyTargets
		case allowLoopback:
			required.Network = controlmatrix.NetworkLoopbackExact
		default:
			required.Network = controlmatrix.NetworkDenied
		}
	}
	return authority.RequiredControls(required)
}

func settleExecutionLease(
	manager *authority.LeaseAuthority,
	lease authority.ExecutionLease,
	status tool.OutcomeStatus,
	reason string,
	completed time.Time,
) (authority.LeaseSnapshot, error) {
	if status == "" {
		status = tool.OutcomeFailed
	}
	if err := manager.Settle(lease, authority.Settlement{
		Status: string(status), Reason: reason, CompletedAt: completed,
	}); err != nil {
		return authority.LeaseSnapshot{}, err
	}
	return manager.Snapshot(lease)
}
