package authority

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type ManagedFileInput struct {
	ID                  string
	Tool                string
	WorkspaceRoot       string
	WorkspaceID         string
	WorkspaceGeneration uint64
	Subject             Subject
	Paths               []string
	MutationDigest      string
	Risk                securitymodel.Risk
}

func BuildManagedFileOperation(
	input ManagedFileInput,
) (ExecutionOperation, error) {
	if err := input.Subject.Validate(); err != nil {
		return ExecutionOperation{}, err
	}
	if strings.TrimSpace(input.ID) == "" ||
		strings.TrimSpace(input.Tool) == "" ||
		strings.TrimSpace(input.WorkspaceRoot) == "" ||
		!validDigest(input.WorkspaceID) ||
		input.WorkspaceGeneration == 0 ||
		!validDigest(input.MutationDigest) ||
		len(input.Paths) == 0 {
		return ExecutionOperation{}, errors.New("managed file operation is incomplete")
	}
	resources := make([]Resource, 0, len(input.Paths))
	for _, path := range input.Paths {
		path = filepath.ToSlash(filepath.Clean(path))
		if path == "" || path == "." || filepath.IsAbs(path) ||
			path == ".." || strings.HasPrefix(path, "../") {
			return ExecutionOperation{}, errors.New("managed file path is invalid")
		}
		resource := Resource{
			Namespace: NamespaceWorkspace,
			RootID:    input.WorkspaceID, RootGeneration: input.WorkspaceGeneration,
			RelativePath: path, Kind: securitymodel.ClassPath.String(), Access: securitymodel.Write,
		}
		if err := resource.Validate(); err != nil {
			return ExecutionOperation{}, err
		}
		resources = append(resources, resource)
	}
	resources = normalizeResources(resources)
	digests := make([]string, 0, len(resources))
	for _, resource := range resources {
		digest, err := resourceDigest(resource)
		if err != nil {
			return ExecutionOperation{}, err
		}
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	operation := ExecutionOperation{
		SchemaVersion: OperationSchemaVersion,
		ID:            input.ID, Tool: input.Tool,
		WorkspaceID:         input.WorkspaceID,
		WorkspaceGeneration: input.WorkspaceGeneration,
		Subject:             input.Subject,
		Effect: EffectContract{
			Kind:                   securitymodel.WorkspaceEdit,
			Reversibility:          securitymodel.Reversible,
			Risk:                   input.Risk,
			WorkspaceTransaction:   WorkspaceTransactionBeforeImage,
			RequireReadBeforeWrite: true,
		},
		Required: RequiredControls{
			FilesystemRead:  securitymodel.FilesystemReadExactPaths,
			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,
			PathIdentity:    securitymodel.PathIdentityDescriptorRelative,
			DurableRecovery: securitymodel.DurableRecoveryExternalJournal,
		},
		Resources: resources,
		File: &FileIntent{
			ResourceDigests: digests, MutationDigest: input.MutationDigest,
		},
	}
	digest, err := operationDigest(operation)
	if err != nil {
		return ExecutionOperation{}, err
	}
	operation.Digest = digest
	return operation, operation.Validate()
}

func BuildManagedFileProfile(
	operation ExecutionOperation,
	revision uint64,
	workspaceRoot string,
) (EffectivePermissionProfile, error) {
	if err := operation.Validate(); err != nil {
		return EffectivePermissionProfile{}, err
	}
	if operation.File == nil || revision == 0 {
		return EffectivePermissionProfile{}, errors.New("managed file profile is incomplete")
	}
	profile := EffectivePermissionProfile{
		SchemaVersion: SchemaVersion, Revision: revision,
		Tool: operation.Tool, Capability: securitymodel.CapabilityWrite,
		Access: securitymodel.Tree,
		Filesystem: FilesystemAuthority{
			WorkspaceRoot: workspaceRoot,
		},
		Process: ProcessAuthority{
			Enforcement: sandbox.EnforcementNone, Backend: "file_broker",
		},
		Controls: securitymodel.Controls{
			FilesystemRead:  securitymodel.FilesystemReadExactPaths,
			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,
			Network:         securitymodel.NetworkDenied,
			ProcessTree:     securitymodel.ProcessTreeUnmanaged,
			CrossProcess:    securitymodel.CrossProcessUnrestricted,
			Syscall:         securitymodel.SyscallUnrestricted,
			IPC:             securitymodel.IPCUnrestricted,
			PathIdentity:    securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin:  securitymodel.ArtifactOriginUnverifiedPath,
			DurableRecovery: securitymodel.DurableRecoveryExternalJournal,
		},
		Provenance: []AuthoritySource{{
			Kind: "file_broker", Value: operation.Subject.ID,
			Digest:   operation.Subject.Digest,
			Revision: operation.Subject.Generation,
		}},
	}
	for _, resource := range operation.Resources {
		profile.Filesystem.WritePaths = append(
			profile.Filesystem.WritePaths,
			filepath.Join(workspaceRoot, filepath.FromSlash(resource.RelativePath)),
		)
	}
	normalize(&profile)
	digest, err := profileDigest(profile)
	if err != nil {
		return EffectivePermissionProfile{}, err
	}
	profile.Digest = digest
	return profile, profile.Validate()
}
