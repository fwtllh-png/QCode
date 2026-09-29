package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

type StateLayout struct {
	Root          string
	WorkspaceID   string
	Control       string
	SandboxHome   string
	ArtifactStage string
}

// ExternalStateDirectory resolves a Runtime state directory and rejects any
// overlap with the untrusted Workspace before callers create state files.
func ExternalStateDirectory(workspace, stateDirectory string) (string, error) {
	workspace, err := canonicalDirectory(workspace)
	if err != nil {
		return "", fmt.Errorf("canonicalize Workspace for state directory: %w", err)
	}
	stateDirectory, err = CanonicalStateDirectory(stateDirectory)
	if err != nil {
		return "", err
	}
	if pathContains(workspace, stateDirectory) ||
		pathContains(stateDirectory, workspace) {
		return "", errors.New(
			"Runtime state directory and Workspace must not overlap",
		)
	}
	return stateDirectory, nil
}

// CanonicalStateDirectory resolves the Supervisor state root before any
// Workspace exists. Each later Workspace still requires an overlap check.
func CanonicalStateDirectory(stateDirectory string) (string, error) {
	if strings.TrimSpace(stateDirectory) == "" {
		return "", errors.New("Runtime state directory is required")
	}
	stateDirectory, err := filepath.Abs(stateDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve Runtime state directory: %w", err)
	}
	stateDirectory, err = pathpolicy.CanonicalAllowMissing(stateDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve Runtime state directory links: %w", err)
	}
	return filepath.Clean(stateDirectory), nil
}

func PrepareStateLayout(
	dataDir, workspace, workspaceID string,
) (StateLayout, error) {
	base, err := ExternalStateDirectory(workspace, dataDir)
	if err != nil {
		return StateLayout{}, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return StateLayout{}, fmt.Errorf("resolve Workspace links: %w", err)
	}
	if workspaceID == "" {
		sum := sha256.Sum256([]byte(filepath.Clean(workspace)))
		workspaceID = hex.EncodeToString(sum[:])
	}
	if !validStateID(workspaceID) {
		return StateLayout{}, errors.New("Workspace state identity is invalid")
	}
	return prepareStateLayout(
		filepath.Join(base, "workspaces", workspaceID),
		workspaceID,
	)
}

func PrepareChildStateLayout(
	parentRoot, childRoot string,
) (StateLayout, error) {
	if parentRoot == "" {
		return StateLayout{}, nil
	}
	sum := sha256.Sum256([]byte(filepath.Clean(childRoot)))
	workspaceID := hex.EncodeToString(sum[:])
	return prepareStateLayout(
		filepath.Join(parentRoot, "children", workspaceID),
		workspaceID,
	)
}

func prepareStateLayout(root, workspaceID string) (StateLayout, error) {
	layout := StateLayout{
		Root: root, WorkspaceID: workspaceID,
		Control:       filepath.Join(root, "control"),
		SandboxHome:   filepath.Join(root, "sandbox-home"),
		ArtifactStage: filepath.Join(root, "artifacts"),
	}
	for name, path := range map[string]string{
		"control": layout.Control, "sandbox-home": layout.SandboxHome,
		"artifacts": layout.ArtifactStage,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return StateLayout{}, fmt.Errorf(
				"create Workspace %s state directory: %w",
				name,
				err,
			)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return StateLayout{}, fmt.Errorf(
				"protect Workspace %s state directory: %w",
				name,
				err,
			)
		}
	}
	return layout, nil
}

func RequireTrustedConfigFile(path, root, label string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("%s requires an external trusted config root", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular non-symlink file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions are too broad", label)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve %s root: %w", label, err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve %s path: %w", label, err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must be inside the Runtime state directory", label)
	}
	return nil
}

func validStateID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 || filepath.Base(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
