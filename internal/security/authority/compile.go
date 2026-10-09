package authority

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type CompileInput struct {
	Runtime    *policy.Runtime
	Invocation policy.Invocation
	// Prepared is the same invocation as Invocation; it supplies the catalog
	// subject, canonical arguments, and trusted binding for the operation.
	Prepared            securitymodel.PreparedInvocation
	Decision            policy.Decision
	Authorized          bool
	Revision            uint64
	Enforcement         sandbox.Enforcement
	Capability          sandbox.Capability
	SandboxPolicy       sandbox.Policy
	WorkspaceID         string
	WorkspaceGeneration uint64
}

// Authority is one compiled execution: the permission ceiling, the controls
// the execution requires, and the operation the lease binds. All three come
// from the same assessed resources in one pass.
type Authority struct {
	Profile   EffectivePermissionProfile
	Required  RequiredControls
	Operation ExecutionOperation
	hostPaths map[string]string
	hostRoots map[string]string
}

// Evidence is what a broker learns after compilation and before the lease: a
// snapshotted artifact or a planned file mutation.
type Evidence struct {
	Artifact           *ArtifactIntent
	FileMutationDigest string
}

func Compile(input CompileInput) (Authority, error) {
	if input.Runtime == nil || !input.Invocation.Validated || !input.Authorized {
		return Authority{}, errors.New("authorized validated policy input is required")
	}
	if input.Decision.Action == policy.ActionDeny ||
		input.Decision.Action == policy.ActionHold {
		return Authority{}, errors.New("denied invocation has no effective authority")
	}
	if input.Revision == 0 || !input.Enforcement.Valid() {
		return Authority{}, errors.New("authority revision and enforcement are required")
	}
	if input.Prepared.Tool != input.Invocation.Tool ||
		input.Prepared.CallID != input.Invocation.CallID ||
		!bytes.Equal(input.Prepared.Arguments, input.Invocation.Arguments) ||
		!input.Prepared.Assessment.Same(input.Invocation.Assessment) {
		return Authority{}, errors.New("prepared invocation does not match the policy invocation")
	}
	assessment := input.Invocation.Assessment
	if assessment.Facets().FullAccess && (input.Runtime.Permission != policy.PermissionBypass ||
		input.Prepared.Subject.Kind != securitymodel.SubjectBuiltin || input.Invocation.Capability() != securitymodel.CapabilityProcess) {
		return Authority{}, errors.New("full access requires a builtin process and Full Access permission")
	}
	resources := assessment.Resources()
	reach := assessedNetworkReach(assessment.Facets())
	profile, err := compileProfile(input, resources, reach)
	if err != nil {
		return Authority{}, err
	}
	required := requiredControls(input.Prepared.Required, assessment.Facets(), reach)
	if assessment.Facets().FullAccess {
		required.FilesystemRead = securitymodel.FilesystemReadUnrestricted
		required.FilesystemWrite = securitymodel.FilesystemWriteUnrestricted
		required.Network = securitymodel.NetworkDirect
	}
	roots := make(map[string]string)
	for _, candidate := range profileHostRoots(profile) {
		roots[candidate] = canonicalRoot(candidate)
	}
	operation, err := buildExecutionOperation(operationInput{
		WorkspaceRoot:          input.SandboxPolicy.WorkspaceRoot,
		WorkspaceID:            input.WorkspaceID,
		WorkspaceGeneration:    input.WorkspaceGeneration,
		Invocation:             input.Prepared,
		Effect:                 assessment.Effect(),
		Journaled:              input.Invocation.Journaled(),
		RequireReadBeforeWrite: input.Invocation.Journaled(),
		Required:               required,
		HostReadRoots:          profileHostRoots(profile),
	}, resources)
	if err != nil {
		return Authority{}, err
	}
	hosts := make(map[string]string)
	for _, item := range operation.Resources {
		if item.Namespace != NamespaceHostToolchain {
			continue
		}
		for _, candidate := range profileHostRoots(profile) {
			root := roots[candidate]
			if digestString(root) == item.RootID {
				hosts[resourceKey(item)] = filepath.Join(root, item.RelativePath)
				break
			}
		}
		if hosts[resourceKey(item)] == "" {
			return Authority{}, errors.New("compiled host resource has no frozen root")
		}
	}
	return Authority{Profile: profile, Required: required, Operation: operation, hostPaths: hosts, hostRoots: roots}, nil
}

// Bind attaches late evidence. Resources are not reinterpreted; only the
// operation intents, its digest, and the artifact-origin requirement change.
func (a Authority) Bind(evidence Evidence) (Authority, error) {
	if err := a.Operation.Validate(); err != nil {
		return Authority{}, err
	}
	result := a.clone()
	result.Operation.Artifact = cloneArtifactIntent(evidence.Artifact)
	if result.Operation.File != nil {
		result.Operation.File.MutationDigest = evidence.FileMutationDigest
	} else if evidence.FileMutationDigest != "" {
		return Authority{}, errors.New("file evidence requires a file intent")
	}
	if evidence.Artifact != nil {
		result.Required.ArtifactOrigin = securitymodel.ArtifactOriginBrokerSnapshot
	}
	result.Operation.Required = result.Required
	return result.finalize()

}

// WithProfile replaces the ceiling with an amended profile of the same tool,
// such as one widened by an approved additional permission. Host resources are
// rebound against the new profile's roots.
func (a Authority) WithProfile(profile EffectivePermissionProfile) (Authority, error) {
	if err := profile.Validate(); err != nil {
		return Authority{}, err
	}
	if profile.Tool != a.Profile.Tool {
		return Authority{}, errors.New("amended profile belongs to another tool")
	}
	if err := a.Operation.Validate(); err != nil {
		return Authority{}, err
	}
	result := a.clone()
	result.Profile = cloneProfile(profile)
	result.hostPaths = make(map[string]string)
	for i, item := range result.Operation.Resources {
		if item.Namespace != NamespaceHostToolchain {
			continue
		}
		path, exists := a.hostPaths[resourceKey(item)]
		if !exists {
			return Authority{}, errors.New("host resource has no frozen path")
		}
		root := ""
		for _, candidate := range profileHostRoots(profile) {
			if frozen, known := a.hostRoots[candidate]; known {
				candidate = frozen
			}
			if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
				continue
			}
			rel, err := filepath.Rel(candidate, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(candidate) > len(root) {
				root = candidate
			}
		}
		if root == "" {
			return Authority{}, errors.New("frozen resource is outside amended host roots")
		}
		item.RootID = digestString(root)
		item.RelativePath, _ = filepath.Rel(root, path)
		result.Operation.Resources[i] = item
		result.hostPaths[resourceKey(item)] = path
	}
	result.Operation.Resources = normalizeResources(result.Operation.Resources)
	if result.Operation.File != nil {
		var digests []string
		for _, item := range result.Operation.Resources {
			if item.Namespace == NamespaceNetwork || item.Namespace == NamespaceProcess || item.Namespace == NamespaceRuntime || (item.Namespace == NamespaceHostConfig && item.Kind == "env") {
				continue
			}
			digest, err := resourceDigest(item)
			if err != nil {
				return Authority{}, err
			}
			digests = append(digests, digest)
		}
		result.Operation.File.ResourceDigests = uniqueSorted(digests)
	}
	return result.finalize()
}

func (a Authority) clone() Authority {
	a.Profile = cloneProfile(a.Profile)
	a.Operation.Resources = append([]Resource(nil), a.Operation.Resources...)
	for i := range a.Operation.Resources {
		a.Operation.Resources[i].Methods = append([]string(nil), a.Operation.Resources[i].Methods...)
	}
	if a.Operation.Process != nil {
		value := *a.Operation.Process
		a.Operation.Process = &value
	}
	if a.Operation.Network != nil {
		value := *a.Operation.Network
		value.Targets = append([]string(nil), value.Targets...)
		a.Operation.Network = &value
	}
	if a.Operation.File != nil {
		value := *a.Operation.File
		value.ResourceDigests = append([]string(nil), value.ResourceDigests...)
		a.Operation.File = &value
	}
	a.Operation.Artifact = cloneArtifactIntent(a.Operation.Artifact)
	return a
}

func (a Authority) finalize() (Authority, error) {
	digest, err := operationDigest(a.Operation)
	if err != nil {
		return Authority{}, err
	}
	a.Operation.Digest = digest
	return a, a.Operation.Validate()
}

func assessedNetworkReach(facets securitymodel.Facets) sandbox.NetworkReach {
	if facets.Network {
		return sandbox.ReachTargets
	}
	if facets.LoopbackReach {
		return sandbox.ReachLoopback
	}
	return sandbox.ReachNone
}

// requiredControls is what a strong-sandbox execution must be enforced with:
// exact write paths with descriptor-relative identity for any path write, and
// the network control matching the declared reach.
func requiredControls(
	declared securitymodel.RequiredControls,
	facets securitymodel.Facets,
	reach sandbox.NetworkReach,
) RequiredControls {
	required := declared
	if !facets.StrongSandbox {
		return RequiredControls(required)
	}
	if facets.WritesWorkspace {
		required.FilesystemWrite = securitymodel.FilesystemWriteExactPaths
		required.PathIdentity = securitymodel.PathIdentityDescriptorRelative
	}
	switch reach {
	case sandbox.ReachTargets:
		required.Network = securitymodel.NetworkProxyTargets
	case sandbox.ReachLoopback:
		required.Network = securitymodel.NetworkLoopbackAny
	default:
		required.Network = securitymodel.NetworkDenied
	}
	return RequiredControls(required)
}

func profileHostRoots(profile EffectivePermissionProfile) []string {
	return append(
		append([]string(nil), profile.Filesystem.ReadRoots...),
		profile.Filesystem.WritePaths...,
	)
}
