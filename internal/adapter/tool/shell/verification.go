package shell

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func applyVerificationDefaults(input *execCommandInput) {
	if input == nil {
		return
	}
	if input.Verification == "" && len(input.CoveredPaths) != 0 {
		input.Verification = "check"
	}
}

func validateVerification(input execCommandInput) error {
	applyVerificationDefaults(&input)
	if input.Verification == "" {
		return nil
	}
	switch input.Verification {
	case "test", "build", "lint", "check":
	default:
		return errors.New("verification must be test, build, lint, or check")
	}
	if len(input.CoveredPaths) == 0 {
		return errors.New("verification requires exact workspace-relative covered_paths")
	}
	for _, path := range input.CoveredPaths {
		if _, ok := verify.CanonicalEvidencePath(path); !ok {
			return fmt.Errorf("invalid verification covered path %q", path)
		}
	}
	return nil
}

func (p *commandProtocol) prepareVerification(input execCommandInput) (*verify.Evidence, error) {
	applyVerificationDefaults(&input)
	if err := validateVerification(input); err != nil {
		return nil, err
	}
	if input.Verification == "" {
		return nil, nil
	}
	directory, err := p.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.Rel(p.workspace.Root(), directory)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(input.CoveredPaths))
	for _, path := range input.CoveredPaths {
		canonical, _ := verify.CanonicalEvidencePath(path)
		paths = append(paths, canonical)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	digest, err := verify.InputDigest(p.workspace.Root(), paths)
	if err != nil {
		return nil, fmt.Errorf("snapshot verification inputs: %w", err)
	}
	// Bind command identity to its full declaration, including cwd and network
	// scope; output limits and polling intervals do not change the command.
	identity, err := json.Marshal(struct {
		Command        string
		CWD            string
		TTY            bool
		TimeoutMS      int64
		NetworkTargets []tool.DeclaredNetworkTarget
		AllowLoopback  bool
	}{
		input.Command, input.CWD, input.TTY, input.TimeoutMS,
		input.NetworkTargets, input.AllowLoopback,
	})
	if err != nil {
		return nil, err
	}
	return &verify.Evidence{
		SchemaVersion: 1, Kind: input.Verification, Status: verify.StatusRunning,
		Command: input.Command, CWD: filepath.ToSlash(cwd),
		CoveredPaths: paths, InputDigest: digest,
		CommandDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(identity)),
	}, nil
}

func attachVerification(result *tool.Result, evidence *verify.Evidence, wait process.SessionWait) {
	if evidence == nil {
		return
	}
	evidence.ExitCode = wait.ExitCode
	if !wait.Running {
		evidence.Status = verify.StatusFailed
		if wait.ExitCode == 0 && !wait.Terminated {
			evidence.Status = verify.StatusPassed
		}
	}
	result.Metadata[verify.EvidenceMetadataKey] = *evidence
}

// invalidateVerificationOnCoveredWrites downgrades otherwise-passing
// evidence when the command changed the inputs it claims to cover.
// Verification is judged after execution, not by an entry-time veto: a
// build that drops artifacts may declare write_paths, but a command that
// rewrites its own covered inputs cannot testify for its own output.
func invalidateVerificationOnCoveredWrites(
	result *tool.Result, evidence *verify.Evidence, workspaceRoot string,
) {
	if evidence == nil || evidence.Status != verify.StatusPassed {
		return
	}
	after, err := verify.InputDigest(workspaceRoot, evidence.CoveredPaths)
	if err == nil && after == evidence.InputDigest {
		return
	}
	evidence.Status = verify.StatusInvalidated
	evidence.InvalidationReason = "covered_paths changed during verification"
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata[verify.EvidenceMetadataKey] = *evidence
}
