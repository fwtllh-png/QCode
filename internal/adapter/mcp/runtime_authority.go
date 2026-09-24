package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/processbroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type RuntimeAuthority struct {
	Workspace           string
	WorkspaceID         string
	WorkspaceGeneration uint64
	Sandbox             sandbox.Backend
	LeaseAuthority      *authority.LeaseAuthority
	ProcessBroker       *processbroker.Broker
	RequireHostTrust    bool
	generation          atomic.Uint64
}

func NewRuntimeAuthority(
	workspace, workspaceID string,
	workspaceGeneration uint64,
	backend sandbox.Backend,
	leases *authority.LeaseAuthority,
) (*RuntimeAuthority, error) {
	if workspace == "" {
		workspace = "."
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	if workspaceID == "" {
		sum := sha256.Sum256([]byte(filepath.Clean(absolute)))
		workspaceID = hex.EncodeToString(sum[:])
	}
	if workspaceGeneration == 0 {
		workspaceGeneration = 1
	}
	if leases == nil {
		return nil, errors.New("lease authority is required")
	}
	broker, err := processbroker.New(leases)
	if err != nil {
		return nil, err
	}
	return &RuntimeAuthority{
		Workspace: absolute, WorkspaceID: workspaceID,
		WorkspaceGeneration: workspaceGeneration,
		Sandbox:             backend, LeaseAuthority: leases, ProcessBroker: broker,
		RequireHostTrust: true,
	}, nil
}

func (r *RuntimeAuthority) Start(
	ctx context.Context,
	name string,
	config ServerConfig,
	environment []string,
) (*processbroker.Lifecycle, error) {
	// environment 是调用方声明的条目（非预合成环境）；进程边界
	// process.NewCommand 负责宿主继承与沙盒覆盖，这里只透传与记账。
	if r == nil || r.LeaseAuthority == nil || r.ProcessBroker == nil {
		return nil, errors.New("MCP Runtime Authority is unavailable")
	}
	if r.RequireHostTrust && !config.HostTrusted {
		return nil, errors.New("MCP stdio lifecycle requires host_trusted=true")
	}
	if config.Authority != nil {
		if err := config.Authority(ctx); err != nil {
			return nil, err
		}
	}
	generation := r.generation.Add(1)
	configDigest, err := serverConfigHash(name, config)
	if err != nil {
		return nil, err
	}
	subject, err := authority.NewManagedProcessSubject(
		authority.SubjectMCPTool,
		name,
		authority.TrustHost,
		generation,
		configDigest,
	)
	if err != nil {
		return nil, err
	}
	directory := config.WorkingDirectory
	if directory == "" {
		directory = r.Workspace
	}
	strong := r.Sandbox != nil
	allowNetwork := mcpNetworkAllowed(config.PermissionProfile)
	allowWrite := mcpWorkspaceWriteAllowed(config.PermissionProfile)
	policyID := ""
	var sandboxPolicy sandbox.Policy
	if bound, ok := sandbox.BackendPolicy(r.Sandbox); ok {
		sandboxPolicy = bound
		policyID = bound.ID
	}
	required := authority.RequiredControls{}
	capability := sandbox.Capability{Backend: "host"}
	enforcement := "none"
	if strong {
		capability = r.Sandbox.Capability()
		enforcement = "strong"
		required = authority.RequiredControls{
			FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,
			Network:        controlmatrix.NetworkDenied,
			ProcessTree:    controlmatrix.ProcessTreeGroupKill,
			PathIdentity:   controlmatrix.PathIdentityDescriptorRelative,
		}
		if allowWrite {
			required.FilesystemWrite = controlmatrix.FilesystemWriteExactPaths
		}
	}
	operation, err := authority.BuildManagedProcessOperation(
		authority.ManagedProcessInput{
			ID:                  "mcp:" + name + ":" + configDigest,
			Tool:                "mcp_lifecycle:" + name,
			WorkspaceID:         r.WorkspaceID,
			WorkspaceGeneration: r.WorkspaceGeneration,
			Subject:             subject, Executable: config.Command,
			Args: config.Args, WorkingDirectory: directory,
			Environment: environment,
			Effect:      authority.ManagedProcessEffect(policy.RiskHigh),
			Required:    required,
		},
	)
	if err != nil {
		return nil, err
	}
	readPaths := mcpExecutableReadPaths(config.Command)
	profile, err := authority.BuildManagedProcessProfile(
		authority.ManagedProfileInput{
			Operation: operation, Revision: generation,
			WorkspaceRoot:      r.Workspace,
			WorkspaceBaseWrite: allowWrite || !strong,
			ReadRoots:          readPaths,
			AllowNetwork:       allowNetwork || !strong,
			NetworkTargets:     mcpNetworkTargets(config.PermissionProfile),
			ManagedProxyPort:   sandboxPolicy.ManagedProxyPort,
			Enforcement:        enforcement, Backend: capability.Backend,
			Controls: sandbox.CommandControls(
				capability,
				sandboxPolicy,
				sandbox.Command{
					WorkspaceReadOnly: !allowWrite,
					DenyNetwork:       !allowNetwork,
				},
			),
		},
	)
	if err != nil {
		return nil, err
	}
	connectTimeout := config.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	expiresAt := time.Now().Add(connectTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	lease, err := r.LeaseAuthority.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: generation, SandboxPolicyID: policyID,
		Attempt: generation, ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}
	validation := authority.LeaseValidation{
		Operation: operation, PolicyRevision: generation,
		WorkspaceID:         operation.WorkspaceID,
		WorkspaceGeneration: operation.WorkspaceGeneration,
		SubjectDigest:       operation.Subject.Digest,
		SubjectGeneration:   operation.Subject.Generation,
		SandboxPolicyID:     policyID, Attempt: generation,
	}
	options := process.Options{
		Path: config.Command, Args: config.Args, Dir: directory,
		Env: environment, Sandbox: r.Sandbox,
		RequireSandbox:       strong,
		WorkspaceReadOnly:    strong && !allowWrite,
		DenyNetwork:          strong && !allowNetwork,
		WorkspaceHiddenPaths: mcpHiddenPaths(r.Workspace),
		AdditionalReadPaths:  readPaths,
	}
	if strong {
		directoryFile, openErr := process.OpenPinnedDirectory(r.Sandbox, directory)
		if openErr != nil {
			_ = r.LeaseAuthority.Revoke(lease)
			_ = r.LeaseAuthority.Release(lease)
			return nil, openErr
		}
		defer directoryFile.Close()
		options.DirFile = directoryFile
	}
	lifecycle, err := r.ProcessBroker.StartLifecycle(
		ctx,
		processbroker.LifecycleRequest{
			Lease: lease, Validation: validation, Options: options,
			Identity: processbroker.Identity{
				SessionID: r.WorkspaceID, ThreadID: "mcp:" + name,
				TurnID: configDigest,
			},
		},
	)
	if err != nil {
		if snapshot, snapshotErr := r.LeaseAuthority.Snapshot(lease); snapshotErr == nil &&
			snapshot.State == authority.LeaseIssued {
			_ = r.LeaseAuthority.Revoke(lease)
		}
		_ = r.LeaseAuthority.Release(lease)
		return nil, err
	}
	return lifecycle, nil
}

func mcpNetworkAllowed(profile *PermissionProfile) bool {
	return mcpCapabilityAllowed(profile, "network")
}

func mcpWorkspaceWriteAllowed(profile *PermissionProfile) bool {
	return mcpCapabilityAllowed(profile, "write")
}

func mcpCapabilityAllowed(profile *PermissionProfile, expected string) bool {
	if profile == nil {
		return false
	}
	for _, capability := range profile.Capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func mcpNetworkTargets(profile *PermissionProfile) []string {
	if profile == nil {
		return nil
	}
	return append([]string(nil), profile.NetworkHosts...)
}

func mcpHiddenPaths(workspace string) []string {
	var paths []string
	for _, name := range []string{
		".agents", ".qcode", ".qcode-worktree", ".codex", ".git",
	} {
		path := filepath.Join(workspace, name)
		if _, err := os.Lstat(path); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}

func mcpExecutableReadPaths(command string) []string {
	if filepath.IsAbs(command) {
		if resolved, err := filepath.EvalSymlinks(command); err == nil {
			command = resolved
		}
		return []string{command}
	}
	return nil
}
