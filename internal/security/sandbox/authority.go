package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// Enforcement is whether an execution runs inside the OS sandbox.
type Enforcement string

const (
	EnforcementStrong Enforcement = "strong"
	EnforcementNone   Enforcement = "none"
)

func (e Enforcement) Valid() bool {
	return e == EnforcementStrong || e == EnforcementNone
}

type ExecutionAuthority struct {
	HostExecution       bool
	FullAccess          bool
	Digest              string
	Enforcement         Enforcement
	WorkspaceRoot       string
	WorkspaceBaseWrite  bool
	ReadPaths           []string
	WorkspaceWritePaths []string
	NetworkTargets      []string
	ManagedProxyPort    uint16
	AllowLoopback       bool
	AllowNetwork        bool
	AllowProcess        bool
	RequiredControls    securitymodel.RequiredControls
	EffectiveControls   securitymodel.Controls
}

func (a ExecutionAuthority) Validate() error {
	if len(a.Digest) != 64 {
		return errors.New("execution authority requires a SHA-256 digest")
	}
	if !a.Enforcement.Valid() {
		return errors.New("execution authority enforcement is invalid")
	}
	if a.HostExecution {
		controls := a.EffectiveControls
		if a.FullAccess || a.Enforcement != EnforcementNone || !a.AllowProcess ||
			!a.WorkspaceBaseWrite || !a.AllowNetwork || a.ManagedProxyPort != 0 ||
			a.AllowLoopback || len(a.WorkspaceWritePaths) != 0 || len(a.NetworkTargets) != 0 ||
			controls.FilesystemRead != securitymodel.FilesystemReadUnrestricted ||
			controls.FilesystemWrite != securitymodel.FilesystemWriteUnrestricted ||
			controls.Network != securitymodel.NetworkDirect || controls.Syscall != securitymodel.SyscallUnrestricted ||
			controls.IPC != securitymodel.IPCUnrestricted || controls.CrossProcess != securitymodel.CrossProcessUnrestricted ||
			controls.ProcessTree != securitymodel.ProcessTreeGroupKill || controls.PathIdentity != securitymodel.PathIdentityDescriptorRelative {
			return errors.New("host execution requires explicit unrestricted process controls without a sandbox")
		}
	}
	if a.FullAccess {
		if a.Enforcement != EnforcementStrong || !a.AllowProcess ||
			a.EffectiveControls.FilesystemRead != securitymodel.FilesystemReadUnrestricted {
			return errors.New("full access requires consistent compiled process controls")
		}
		if a.WorkspaceBaseWrite {
			if len(a.WorkspaceWritePaths) != 0 || a.EffectiveControls.FilesystemWrite != securitymodel.FilesystemWriteUnrestricted {
				return errors.New("full access write scope is inconsistent")
			}
		} else if len(a.WorkspaceWritePaths) == 0 || a.EffectiveControls.FilesystemWrite != securitymodel.FilesystemWriteExactPaths {
			return errors.New("full access requires explicit restricted write paths")
		}
		switch a.EffectiveControls.Network {
		case securitymodel.NetworkDirect:
			if !a.AllowNetwork || a.ManagedProxyPort != 0 || a.AllowLoopback || len(a.NetworkTargets) != 0 {
				return errors.New("full access direct network has scoped grants")
			}
		case securitymodel.NetworkLoopbackAny:
			if !a.AllowNetwork || !a.LoopbackOnly() {
				return errors.New("full access loopback scope is inconsistent")
			}
		case securitymodel.NetworkProxyTargets:
			if !a.AllowNetwork || a.ManagedProxyPort == 0 || len(a.NetworkTargets) == 0 {
				return errors.New("full access proxy scope is inconsistent")
			}
		case securitymodel.NetworkDenied:
			if a.AllowNetwork || a.ManagedProxyPort != 0 {
				return errors.New("full access network denial is inconsistent")
			}
		default:
			return errors.New("full access network control is invalid")
		}
	}
	if a.Enforcement == EnforcementStrong && strings.TrimSpace(a.WorkspaceRoot) == "" {
		return errors.New("strong execution authority requires a workspace")
	}
	if err := a.RequiredControls.Validate(); err != nil {
		return err
	}
	if a.EffectiveControls != (securitymodel.Controls{}) {
		if err := a.EffectiveControls.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// RelocateWorkspace projects the same grants into a trusted isolated copy.
// The caller supplies the original workspace identity; no write grant may
// escape it. Network, process and required controls remain unchanged.
func (a ExecutionAuthority) RelocateWorkspace(source, target string) (ExecutionAuthority, error) {
	if err := a.Validate(); err != nil {
		return ExecutionAuthority{}, err
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) ||
		filepath.Clean(a.WorkspaceRoot) != filepath.Clean(source) {
		return ExecutionAuthority{}, errors.New("isolated authority requires the original workspace identity and an absolute target")
	}
	relocate := func(path string) (string, bool) {
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return path, false
		}
		return filepath.Join(target, relative), true
	}
	result := a
	result.WorkspaceRoot = filepath.Clean(target)
	result.ReadPaths = slices.Clone(a.ReadPaths)
	for index, path := range result.ReadPaths {
		result.ReadPaths[index], _ = relocate(path)
	}
	result.WorkspaceWritePaths = slices.Clone(a.WorkspaceWritePaths)
	for index, path := range result.WorkspaceWritePaths {
		mapped, ok := relocate(path)
		if !ok {
			return ExecutionAuthority{}, fmt.Errorf("isolated write grant %q is outside the original workspace", path)
		}
		result.WorkspaceWritePaths[index] = mapped
	}
	result.NetworkTargets = slices.Clone(a.NetworkTargets)
	// Bind the projection to both the source authority digest and new root.
	encoded, err := json.Marshal(result)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	digest := sha256.Sum256(encoded)
	result.Digest = hex.EncodeToString(digest[:])
	return result, result.Validate()
}

// VerifyPrepared checks the controls a backend reports for a prepared command
// against this compiled authority. Required controls must hold, and the
// prepared network control may be stricter than, but never broader than, the
// compiled one. Both sides come from NetworkControl, so a mismatch means the
// backend and the authority disagree about the same execution.
func (a ExecutionAuthority) VerifyPrepared(
	prepared securitymodel.Controls,
) (securitymodel.Controls, error) {
	if a.EffectiveControls != (securitymodel.Controls{}) {
		prepared.ArtifactOrigin = a.EffectiveControls.ArtifactOrigin
		prepared.DurableRecovery = a.EffectiveControls.DurableRecovery
	}
	if err := a.RequiredControls.SatisfiedBy(prepared); err != nil {
		return prepared, err
	}
	if a.Enforcement == EnforcementStrong && a.EffectiveControls.Network != "" {
		ceiling := securitymodel.RequiredControls{Network: a.EffectiveControls.Network}
		if err := ceiling.SatisfiedBy(prepared); err != nil {
			return prepared, fmt.Errorf("prepared network exceeds the compiled authority: %w", err)
		}
	}
	if a.FullAccess {
		// These dimensions describe both grants and restrictions. A stronger
		// sandbox can still break the authorized workload (browser/PTY/IPC).
		if prepared.FilesystemRead != a.EffectiveControls.FilesystemRead ||
			prepared.FilesystemWrite != a.EffectiveControls.FilesystemWrite ||
			prepared.Network != a.EffectiveControls.Network ||
			prepared.Syscall != a.EffectiveControls.Syscall || prepared.IPC != a.EffectiveControls.IPC {
			return prepared, errors.New("prepared controls differ from the compiled Full Access grants")
		}
	}
	return prepared, nil
}

func (a ExecutionAuthority) AllowsWritePaths(paths []string) bool {
	if a.WorkspaceBaseWrite {
		return true
	}
	allowed := append([]string(nil), a.WorkspaceWritePaths...)
	for index := range allowed {
		allowed[index] = filepath.Clean(allowed[index])
	}
	slices.Sort(allowed)
	for _, path := range paths {
		if !slices.Contains(allowed, filepath.Clean(path)) {
			return false
		}
	}
	return true
}

func (a ExecutionAuthority) DeniedReadPath(paths []string) (string, bool) {
	return deniedPath(paths, a.ReadPaths)
}

func (a ExecutionAuthority) DeniedWritePath(paths []string) (string, bool) {
	if a.WorkspaceBaseWrite {
		return "", false
	}
	return deniedPath(paths, a.WorkspaceWritePaths)
}

// LoopbackOnly reports a localhost bind/connect grant that does not claim the
// managed egress proxy. Outbound HTTP(S) targets still require a matching
// proxy port on the effective profile.
func (a ExecutionAuthority) LoopbackOnly() bool {
	return a.AllowLoopback && a.ManagedProxyPort == 0 && len(a.NetworkTargets) == 0
}

func deniedPath(requested, allowed []string) (string, bool) {
	canonical := append([]string(nil), allowed...)
	for index := range canonical {
		canonical[index] = filepath.Clean(canonical[index])
	}
	slices.Sort(canonical)
	for _, path := range requested {
		candidate := filepath.Clean(path)
		permitted := slices.Contains(canonical, candidate)
		for _, root := range canonical {
			relative, err := filepath.Rel(root, candidate)
			if err == nil && relative != ".." &&
				!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				permitted = true
				break
			}
		}
		if !permitted {
			return path, true
		}
	}
	return "", false
}

type executionAuthorityKey struct{}

func WithExecutionAuthority(
	ctx context.Context,
	authority ExecutionAuthority,
) (context.Context, error) {
	if err := authority.Validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, executionAuthorityKey{}, authority), nil
}

func ExecutionAuthorityFromContext(ctx context.Context) (ExecutionAuthority, bool) {
	if ctx == nil {
		return ExecutionAuthority{}, false
	}
	authority, ok := ctx.Value(executionAuthorityKey{}).(ExecutionAuthority)
	return authority, ok
}
