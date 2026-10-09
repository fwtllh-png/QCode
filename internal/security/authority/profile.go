// Package authority compiles policy intent into an immutable execution ceiling.
package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const SchemaVersion = 5

type FilesystemAuthority struct {
	WorkspaceRoot      string   `json:"workspace_root"`
	ReadRoots          []string `json:"read_roots,omitempty"`
	WritePaths         []string `json:"write_paths,omitempty"`
	DeniedWriteRoots   []string `json:"denied_write_roots,omitempty"`
	WorkspaceBaseWrite bool     `json:"workspace_base_write,omitempty"`
}

// NetworkAuthority lists the declared reach. The enforced network mode is
// EffectivePermissionProfile.Controls.Network.
type NetworkAuthority struct {
	Targets   []string `json:"targets,omitempty"`
	ProxyPort uint16   `json:"proxy_port,omitempty"`
	Loopback  bool     `json:"loopback,omitempty"`
}

type ProcessAuthority struct {
	FullAccess  bool                `json:"full_access,omitempty"`
	Allowed     bool                `json:"allowed"`
	Enforcement sandbox.Enforcement `json:"enforcement"`
	Backend     string              `json:"backend"`
}

type AuthoritySource struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Digest   string `json:"digest,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

type EffectivePermissionProfile struct {
	SchemaVersion int                      `json:"schema_version"`
	Revision      uint64                   `json:"revision"`
	Tool          string                   `json:"tool"`
	Capability    securitymodel.Capability `json:"capability"`
	Access        securitymodel.Access     `json:"access"`
	Filesystem    FilesystemAuthority      `json:"filesystem"`
	Network       NetworkAuthority         `json:"network"`
	Process       ProcessAuthority         `json:"process"`
	Controls      securitymodel.Controls   `json:"controls"`
	Provenance    []AuthoritySource        `json:"provenance"`
	Digest        string                   `json:"digest"`
}

func compileProfile(
	input CompileInput,
	resources []securitymodel.Resource,
	reach sandbox.NetworkReach,
) (EffectivePermissionProfile, error) {
	capability := input.Capability
	profile := EffectivePermissionProfile{
		SchemaVersion: SchemaVersion,
		Revision:      input.Revision,
		Tool:          input.Invocation.Tool,
		Capability:    input.Invocation.Capability(),
		Access:        input.Invocation.Access(),
		Filesystem: FilesystemAuthority{
			WorkspaceRoot: input.SandboxPolicy.WorkspaceRoot,
		},
		Process: ProcessAuthority{
			Enforcement: input.Enforcement,
			Backend:     capability.Backend,
		},
		Controls: sandbox.EffectiveControls(
			capability,
			input.SandboxPolicy,
		),
	}
	compileResources(&profile, input.Invocation, resources)
	compileSandboxCeiling(&profile, input, reach)
	if input.Invocation.Assessment.Facets().FullAccess {
		profile.Access = securitymodel.Write
		profile.Process.FullAccess = true
		profile.Filesystem.WorkspaceBaseWrite = true
		profile.Filesystem.ReadRoots = []string{"/"}
		profile.Controls.FilesystemRead = securitymodel.FilesystemReadUnrestricted
		profile.Controls.FilesystemWrite = securitymodel.FilesystemWriteUnrestricted
		profile.Controls.Network = securitymodel.NetworkDirect
		profile.Network = NetworkAuthority{}
	}
	profile.Provenance = provenance(input)
	normalize(&profile)
	digest, err := profileDigest(profile)
	if err != nil {
		return EffectivePermissionProfile{}, err
	}
	profile.Digest = digest
	return profile, profile.Validate()
}

func (p EffectivePermissionProfile) Validate() error {
	if p.SchemaVersion != SchemaVersion || p.Revision == 0 ||
		p.Tool == "" || p.Capability == "" || p.Digest == "" {
		return errors.New("effective permission profile is incomplete")
	}
	expected, err := profileDigest(p)
	if err != nil {
		return err
	}
	if expected != p.Digest {
		return errors.New("effective permission profile digest mismatch")
	}
	if !p.Process.Enforcement.Valid() {
		return errors.New("effective permission profile enforcement is invalid")
	}
	if p.Process.FullAccess && (p.Process.Enforcement != sandbox.EnforcementStrong || !p.Process.Allowed ||
		!p.Filesystem.WorkspaceBaseWrite || p.Controls.Network != securitymodel.NetworkDirect || p.Network.ProxyPort != 0 ||
		p.Controls.FilesystemRead != securitymodel.FilesystemReadUnrestricted || p.Controls.FilesystemWrite != securitymodel.FilesystemWriteUnrestricted) {
		return errors.New("full access profile has inconsistent process controls")
	}
	if p.Process.Enforcement == sandbox.EnforcementStrong && p.Process.Backend == "" {
		return errors.New("controlled profile has no sandbox backend")
	}
	if err := p.Controls.Validate(); err != nil {
		return fmt.Errorf("effective controls: %w", err)
	}
	return nil
}

func (p EffectivePermissionProfile) executionAuthority(
	required RequiredControls,
) sandbox.ExecutionAuthority {
	return sandbox.ExecutionAuthority{
		FullAccess:    p.Process.FullAccess,
		Digest:        p.Digest,
		Enforcement:   p.Process.Enforcement,
		WorkspaceRoot: p.Filesystem.WorkspaceRoot,
		WorkspaceBaseWrite: p.Filesystem.WorkspaceBaseWrite ||
			p.Process.Enforcement == sandbox.EnforcementNone,
		ReadPaths:           append([]string(nil), p.Filesystem.ReadRoots...),
		WorkspaceWritePaths: append([]string(nil), p.Filesystem.WritePaths...),
		NetworkTargets:      append([]string(nil), p.Network.Targets...),
		ManagedProxyPort:    p.Network.ProxyPort,
		AllowLoopback:       p.Network.Loopback,
		AllowNetwork:        p.Controls.Network != securitymodel.NetworkDenied,
		AllowProcess:        p.Process.Allowed,
		RequiredControls:    securitymodel.RequiredControls(required),
		EffectiveControls:   p.Controls,
	}
}

func (p EffectivePermissionProfile) ExecutionAuthorityFor(
	operation ExecutionOperation,
) sandbox.ExecutionAuthority {
	result := p.executionAuthority(operation.Required)
	result.EffectiveControls = effectiveControls(p, operation)
	return result
}

// compileResources maps typed resources onto the profile. A tree write at the
// Workspace root opens the Workspace base; any other tree write stays an exact
// write root. A single-file write opens the base only when the Workspace
// journal records it.
func compileResources(
	profile *EffectivePermissionProfile,
	invocation policy.Invocation,
	resources []securitymodel.Resource,
) {
	workspaceRoot := canonicalRoot(profile.Filesystem.WorkspaceRoot)
	for _, resource := range resources {
		switch resource.Class {
		case securitymodel.ClassPath:
			switch {
			case !resource.Writes():
				profile.Filesystem.ReadRoots = append(profile.Filesystem.ReadRoots, resource.Path)
			case resource.Tree && workspaceRoot != "" && canonicalRoot(resource.Path) == workspaceRoot:
				profile.Filesystem.WorkspaceBaseWrite = true
			case !resource.Tree && invocation.Journaled():
				profile.Filesystem.WorkspaceBaseWrite = true
			default:
				profile.Filesystem.WritePaths = append(profile.Filesystem.WritePaths, resource.Path)
			}
		case securitymodel.ClassLoopback:
			profile.Network.Loopback = true
		case securitymodel.ClassNetwork:
			if resource.Network != nil {
				profile.Network.Targets = append(profile.Network.Targets, resource.Network.Key())
			}
		case securitymodel.ClassProcess:
			profile.Process.Allowed = true
		}
	}
	if invocation.Capability() == securitymodel.CapabilityProcess ||
		invocation.Capability() == securitymodel.CapabilityExternal ||
		invocation.StrongSandbox() {
		profile.Process.Allowed = true
	}
}

func compileSandboxCeiling(
	profile *EffectivePermissionProfile,
	input CompileInput,
	reach sandbox.NetworkReach,
) {
	if input.Enforcement == sandbox.EnforcementNone {
		profile.Process.Backend = "none"
		profile.Controls = unrestrictedControls()
		return
	}
	policyValue := input.SandboxPolicy
	profile.Filesystem.ReadRoots = append(
		profile.Filesystem.ReadRoots,
		policyValue.WorkspaceRoot,
	)
	profile.Filesystem.ReadRoots = append(
		profile.Filesystem.ReadRoots,
		policyValue.RuntimeReadRoots...,
	)
	profile.Filesystem.ReadRoots = append(
		profile.Filesystem.ReadRoots,
		policyValue.HostReadRoots...,
	)
	profile.Filesystem.ReadRoots = append(
		profile.Filesystem.ReadRoots,
		policyValue.HostReadFiles...,
	)
	for _, name := range pathpolicy.ControlPlaneNames() {
		profile.Filesystem.DeniedWriteRoots = append(
			profile.Filesystem.DeniedWriteRoots,
			filepath.Join(policyValue.WorkspaceRoot, name),
		)
	}
	profile.Controls.Network = sandbox.NetworkControl(input.Capability, policyValue, reach)
	if profile.Controls.Network == securitymodel.NetworkProxyTargets {
		profile.Network.ProxyPort = policyValue.ManagedProxyPort
	}
}

func canonicalRoot(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	canonical, err := pathpolicy.CanonicalAllowMissing(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return canonical
}

func unrestrictedControls() securitymodel.Controls {
	return securitymodel.Controls{
		FilesystemRead:  securitymodel.FilesystemReadUnrestricted,
		FilesystemWrite: securitymodel.FilesystemWriteUnrestricted,
		Network:         securitymodel.NetworkDirect,
		ProcessTree:     securitymodel.ProcessTreeUnmanaged,
		CrossProcess:    securitymodel.CrossProcessUnrestricted,
		Syscall:         securitymodel.SyscallUnrestricted,
		IPC:             securitymodel.IPCUnrestricted,
		PathIdentity:    securitymodel.PathIdentityLexical,
		ArtifactOrigin:  securitymodel.ArtifactOriginUnverifiedPath,
		DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
	}
}

func provenance(input CompileInput) []AuthoritySource {
	sources := []AuthoritySource{
		{Kind: "policy", Value: "snapshot", Revision: input.Runtime.Revision},
		{Kind: "mode", Value: string(input.Runtime.Mode)},
		{Kind: "permission", Value: string(input.Runtime.Permission)},
		{Kind: "tool", Value: input.Invocation.Tool},
		{Kind: "constitution", Value: "rules", Digest: digestJSON(input.Runtime.Constitution)},
		{Kind: "managed", Value: "rules", Digest: digestJSON(input.Runtime.Grants)},
		{Kind: "user", Value: "rules", Digest: digestJSON(input.Runtime.User)},
		{Kind: "repository", Value: "rules", Digest: digestJSON(input.Runtime.Repository)},
		{Kind: "authorization", Value: string(input.Decision.Action)},
		{Kind: "decision_layer", Value: string(input.Decision.Layer)},
		{Kind: "sandbox", Value: string(input.Enforcement), Digest: input.SandboxPolicy.ID},
	}
	if grant, ok := input.Runtime.ManagedGrant(input.Invocation); ok {
		sources = append(sources, AuthoritySource{
			Kind: "grant", Value: "managed", Digest: digestJSON(grant),
		})
	}
	if grant, ok := policy.GrantForInvocation(input.Invocation); ok {
		sources = append(sources, AuthoritySource{
			Kind: "grant_key", Value: grant.Kind, Digest: grant.Key,
		})
	}
	return sources
}

func normalize(profile *EffectivePermissionProfile) {
	profile.Filesystem.ReadRoots = uniqueSorted(profile.Filesystem.ReadRoots)
	profile.Filesystem.WritePaths = uniqueSorted(profile.Filesystem.WritePaths)
	profile.Filesystem.DeniedWriteRoots = uniqueSorted(profile.Filesystem.DeniedWriteRoots)
	profile.Network.Targets = uniqueSorted(profile.Network.Targets)
	sort.Slice(profile.Provenance, func(i, j int) bool {
		if profile.Provenance[i].Kind == profile.Provenance[j].Kind {
			return profile.Provenance[i].Value < profile.Provenance[j].Value
		}
		return profile.Provenance[i].Kind < profile.Provenance[j].Kind
	})
}

func uniqueSorted(values []string) []string {
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value != "" && (len(result) == 0 || result[len(result)-1] != value) {
			result = append(result, value)
		}
	}
	return result
}

func profileDigest(profile EffectivePermissionProfile) (string, error) {
	profile.Digest = ""
	encoded, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
