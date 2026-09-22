// Package authority compiles policy intent into an immutable execution ceiling.
package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const SchemaVersion = 3

type FilesystemAuthority struct {
	WorkspaceRoot      string   `json:"workspace_root"`
	ReadRoots          []string `json:"read_roots,omitempty"`
	WritePaths         []string `json:"write_paths,omitempty"`
	DeniedWriteRoots   []string `json:"denied_write_roots,omitempty"`
	WorkspaceBaseWrite bool     `json:"workspace_base_write,omitempty"`
}

type NetworkAuthority struct {
	Mode      string   `json:"mode"`
	Targets   []string `json:"targets,omitempty"`
	ProxyPort uint16   `json:"proxy_port,omitempty"`
	Loopback  bool     `json:"loopback,omitempty"`
}

type ProcessAuthority struct {
	Allowed     bool   `json:"allowed"`
	Enforcement string `json:"enforcement"`
	Backend     string `json:"backend"`
}

type AuthoritySource struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Digest   string `json:"digest,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

type EffectivePermissionProfile struct {
	SchemaVersion int                  `json:"schema_version"`
	Revision      uint64               `json:"revision"`
	Tool          string               `json:"tool"`
	Capability    tool.Capability      `json:"capability"`
	Access        tool.AccessMode      `json:"access"`
	Filesystem    FilesystemAuthority  `json:"filesystem"`
	Network       NetworkAuthority     `json:"network"`
	Process       ProcessAuthority     `json:"process"`
	Controls      controlmatrix.Matrix `json:"controls"`
	Provenance    []AuthoritySource    `json:"provenance"`
	Digest        string               `json:"digest"`
}

type CompileInput struct {
	Runtime       *policy.Runtime
	Invocation    policy.Invocation
	Decision      policy.Decision
	Authorized    bool
	Revision      uint64
	Enforcement   string
	Capability    sandbox.Capability
	SandboxPolicy sandbox.Policy
}

func Compile(input CompileInput) (EffectivePermissionProfile, error) {
	if input.Runtime == nil || !input.Invocation.Validated || !input.Authorized {
		return EffectivePermissionProfile{}, errors.New("authorized validated policy input is required")
	}
	if input.Decision.Action == policy.ActionDeny ||
		input.Decision.Action == policy.ActionHold {
		return EffectivePermissionProfile{}, errors.New("denied invocation has no effective authority")
	}
	if input.Revision == 0 || (input.Enforcement != "strong" && input.Enforcement != "none") {
		return EffectivePermissionProfile{}, errors.New("authority revision and enforcement are required")
	}
	capability := input.Capability
	profile := EffectivePermissionProfile{
		SchemaVersion: SchemaVersion,
		Revision:      input.Revision,
		Tool:          input.Invocation.Tool,
		Capability:    input.Invocation.Capability,
		Access:        input.Invocation.Access,
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
	compileResources(&profile, input.Invocation)
	compileSandboxCeiling(&profile, input)
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
	if p.Process.Enforcement == "strong" && p.Process.Backend == "" {
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
		Digest:        p.Digest,
		Enforcement:   p.Process.Enforcement,
		WorkspaceRoot: p.Filesystem.WorkspaceRoot,
		WorkspaceBaseWrite: p.Filesystem.WorkspaceBaseWrite ||
			p.Process.Enforcement == "none",
		ReadPaths:           append([]string(nil), p.Filesystem.ReadRoots...),
		WorkspaceWritePaths: append([]string(nil), p.Filesystem.WritePaths...),
		NetworkTargets:      append([]string(nil), p.Network.Targets...),
		ManagedProxyPort:    p.Network.ProxyPort,
		AllowLoopback:       p.Network.Loopback,
		AllowNetwork:        p.Network.Mode != "denied",
		AllowProcess:        p.Process.Allowed,
		RequiredControls:    controlmatrix.Requirements(required),
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

func compileResources(profile *EffectivePermissionProfile, invocation policy.Invocation) {
	for _, resource := range invocation.Resources {
		value := resource.Path
		if value == "" {
			value = resource.ID
		}
		switch resource.Kind {
		case "file", "directory", "repo", "workspace":
			if resource.Access == tool.AccessRead {
				profile.Filesystem.ReadRoots = append(profile.Filesystem.ReadRoots, value)
			} else if resource.Kind == "directory" ||
				(resource.Kind == "file" && resource.Tree) {
				profile.Filesystem.WritePaths = append(profile.Filesystem.WritePaths, value)
			} else if resource.Kind != "file" || invocation.Journaled {
				profile.Filesystem.WorkspaceBaseWrite = true
			} else {
				profile.Filesystem.WritePaths = append(profile.Filesystem.WritePaths, value)
			}
		case "host", "url":
			if resource.Kind == "host" && resource.Protocol == "loopback" {
				profile.Network.Loopback = true
				profile.Network.Targets = append(
					profile.Network.Targets,
					"loopback://localhost:0",
				)
				continue
			}
			target, ok := policy.ParseNetworkTarget(value)
			if resource.Kind == "host" && resource.Protocol != "" {
				target = policy.NetworkTarget{
					Host: resource.ID, Protocol: resource.Protocol,
					Port: resource.Port,
				}
				ok = true
			}
			if ok {
				profile.Network.Targets = append(profile.Network.Targets, networkKey(target))
			}
		case "process":
			profile.Process.Allowed = true
		}
	}
	if invocation.Capability == tool.CapabilityProcess ||
		invocation.Capability == tool.CapabilityExternal ||
		invocation.Sandbox == tool.SandboxStrong {
		profile.Process.Allowed = true
	}
}

func networkKey(target policy.NetworkTarget) string {
	return target.Protocol + "://" + net.JoinHostPort(
		target.Host,
		strconv.Itoa(int(target.Port)),
	)
}

func compileSandboxCeiling(profile *EffectivePermissionProfile, input CompileInput) {
	if input.Enforcement == "none" {
		profile.Network.Mode = "unrestricted"
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
	for _, name := range []string{
		".agents", ".qcode", ".qcode-worktree", ".git",
	} {
		profile.Filesystem.DeniedWriteRoots = append(
			profile.Filesystem.DeniedWriteRoots,
			filepath.Join(policyValue.WorkspaceRoot, name),
		)
	}
	hasNetworkTargets := false
	for _, target := range profile.Network.Targets {
		if !strings.HasPrefix(target, "loopback://") {
			hasNetworkTargets = true
			break
		}
	}
	desiredMode := "denied"
	desiredControl := controlmatrix.NetworkDenied
	switch {
	case !hasNetworkTargets && !profile.Network.Loopback:
	case profile.Network.Loopback && !hasNetworkTargets:
		desiredMode = "loopback"
		desiredControl = controlmatrix.NetworkLoopbackExact
	case policyValue.ManagedProxyPort != 0:
		desiredMode = "managed"
		desiredControl = controlmatrix.NetworkProxyTargets
		profile.Network.ProxyPort = policyValue.ManagedProxyPort
	case policyValue.AllowNetwork:
		desiredMode = "direct"
		desiredControl = controlmatrix.NetworkDirect
	}
	if sandbox.CanEnforceNetwork(input.Capability, desiredControl) {
		profile.Network.Mode = desiredMode
		profile.Controls.Network = desiredControl
	} else {
		profile.Network.Mode = string(profile.Controls.Network)
		profile.Network.ProxyPort = 0
	}
}

func unrestrictedControls() controlmatrix.Matrix {
	return controlmatrix.Matrix{
		FilesystemRead:  controlmatrix.FilesystemReadUnrestricted,
		FilesystemWrite: controlmatrix.FilesystemWriteUnrestricted,
		Network:         controlmatrix.NetworkDirect,
		ProcessTree:     controlmatrix.ProcessTreeUnmanaged,
		CrossProcess:    controlmatrix.CrossProcessUnrestricted,
		Syscall:         controlmatrix.SyscallUnrestricted,
		IPC:             controlmatrix.IPCUnrestricted,
		PathIdentity:    controlmatrix.PathIdentityLexical,
		ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
		DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
	}
}

func provenance(input CompileInput) []AuthoritySource {
	sources := []AuthoritySource{
		{Kind: "policy", Value: "snapshot", Revision: input.Runtime.Revision},
		{Kind: "mode", Value: string(input.Runtime.Mode)},
		{Kind: "permission", Value: string(input.Runtime.Permission)},
		{Kind: "tool", Value: input.Invocation.Tool},
		{Kind: "managed", Value: "rules", Digest: digestJSON(input.Runtime.Grants)},
		{Kind: "user", Value: "rules", Digest: digestJSON(input.Runtime.User)},
		{Kind: "repository", Value: "rules", Digest: digestJSON(input.Runtime.Repository)},
		{Kind: "authorization", Value: string(input.Decision.Action)},
		{Kind: "sandbox", Value: input.Enforcement, Digest: input.SandboxPolicy.ID},
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
