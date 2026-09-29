package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/resource"
)

type ExecutionAuthority struct {
	Digest              string
	Enforcement         string
	WorkspaceRoot       string
	WorkspaceBaseWrite  bool
	ReadPaths           []string
	WorkspaceWritePaths []string
	NetworkTargets      []string
	ManagedProxyPort    uint16
	AllowLoopback       bool
	AllowNetwork        bool
	AllowProcess        bool
	RequiredControls    controlmatrix.Requirements
	EffectiveControls   controlmatrix.Matrix
}

func (a ExecutionAuthority) Validate() error {
	if len(a.Digest) != 64 {
		return errors.New("execution authority requires a SHA-256 digest")
	}
	if a.Enforcement != "strong" && a.Enforcement != "none" {
		return errors.New("execution authority enforcement is invalid")
	}
	if a.Enforcement == "strong" && strings.TrimSpace(a.WorkspaceRoot) == "" {
		return errors.New("strong execution authority requires a workspace")
	}
	if err := a.RequiredControls.Validate(); err != nil {
		return err
	}
	if a.EffectiveControls != (controlmatrix.Matrix{}) {
		if err := a.EffectiveControls.Validate(); err != nil {
			return err
		}
	}
	return nil
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
	if !a.AllowLoopback || a.ManagedProxyPort != 0 {
		return false
	}
	for _, target := range a.NetworkTargets {
		if !resource.IsLoopbackTarget(target) {
			return false
		}
	}
	return true
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
