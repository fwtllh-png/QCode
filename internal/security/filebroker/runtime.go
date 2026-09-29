package filebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/effect"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Runtime struct {
	workspace   *sandbox.Workspace
	workspaceID string
	authority   *authority.LeaseAuthority
	broker      *Broker
	leaseTTL    time.Duration
	sequence    atomic.Uint64
}

func NewRuntime(
	workspace *sandbox.Workspace,
	manager *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*Runtime, error) {
	if workspace == nil || manager == nil || leaseTTL <= 0 {
		return nil, errors.New("File Broker Runtime requires Workspace, Authority, and Lease TTL")
	}
	broker, err := New(workspace, manager)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(workspace.Root())))
	return &Runtime{
		workspace: workspace, workspaceID: hex.EncodeToString(sum[:]),
		authority: manager, broker: broker, leaseTTL: leaseTTL,
	}, nil
}

func (r *Runtime) Commit(
	ctx context.Context,
	toolName string,
	plan Plan,
	journal Journal,
) (Result, error) {
	if r == nil || r.broker == nil {
		return Result{}, errors.New("File Broker Runtime is required")
	}
	if len(plan.Entries) == 0 {
		return Result{}, errors.New("managed file transaction has no changes")
	}
	sequence := r.sequence.Add(1)
	subject, err := authority.NewManagedProcessSubject(
		authority.SubjectHost,
		toolName,
		authority.TrustHost,
		sequence,
		struct {
			PlanDigest string   `json:"plan_digest"`
			Paths      []string `json:"paths"`
		}{PlanDigest: plan.Digest, Paths: planPaths(plan)},
	)
	if err != nil {
		return Result{}, err
	}
	operation, err := authority.BuildManagedFileOperation(
		authority.ManagedFileInput{
			ID:   fmt.Sprintf("%s-%d", toolName, sequence),
			Tool: toolName, WorkspaceRoot: r.workspace.Root(),
			WorkspaceID: r.workspaceID, WorkspaceGeneration: 1,
			Subject: subject, Paths: planPaths(plan),
			MutationDigest: plan.Digest, Risk: effect.RiskHigh,
		},
	)
	if err != nil {
		return Result{}, err
	}
	profile, err := authority.BuildManagedFileProfile(
		operation, sequence, r.workspace.Root(),
	)
	if err != nil {
		return Result{}, err
	}
	expiresAt := time.Now().Add(r.leaseTTL)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	lease, err := r.authority.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 1, Attempt: sequence, ExpiresAt: expiresAt,
	})
	if err != nil {
		return Result{}, err
	}
	terminal := false
	defer func() {
		if terminal {
			_ = r.authority.Release(lease)
			return
		}
		if snapshot, snapshotErr := r.authority.Snapshot(lease); snapshotErr == nil &&
			snapshot.State == authority.LeaseIssued {
			_ = r.authority.Revoke(lease)
			_ = r.authority.Release(lease)
		}
	}()
	result, err := r.broker.Commit(ctx, Request{
		Lease: lease,
		Validation: authority.LeaseValidation{
			Operation: operation, PolicyRevision: 1,
			WorkspaceID: r.workspaceID, WorkspaceGeneration: 1,
			SubjectDigest:     subject.Digest,
			SubjectGeneration: subject.Generation,
			Attempt:           sequence,
		},
		Plan: plan, Journal: journal,
	})
	terminal = true
	return result, err
}

func planPaths(plan Plan) []string {
	paths := make([]string, len(plan.Entries))
	for index, entry := range plan.Entries {
		paths[index] = entry.Path
	}
	return paths
}
