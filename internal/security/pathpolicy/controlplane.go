package pathpolicy

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var ErrProtectedPath = errors.New("security control-plane path is protected")

type ControlPlaneClassification struct {
	Root     string
	Relative string
}

type ControlPlane struct {
	workspace string
}

func NewControlPlane(workspace string) (*ControlPlane, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("control-plane workspace is required")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve control-plane workspace: %w", err)
	}
	return &ControlPlane{workspace: filepath.Clean(resolved)}, nil
}

// ControlPlaneWithin classifies against a workspace root the caller has already
// canonicalized. It performs no I/O.
func ControlPlaneWithin(root string) (*ControlPlane, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("control-plane workspace %q is not a canonical absolute path", root)
	}
	return &ControlPlane{workspace: root}, nil
}

func (c *ControlPlane) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

func (c *ControlPlane) Classify(path string) (ControlPlaneClassification, bool, error) {
	if c == nil {
		return ControlPlaneClassification{}, false, errors.New("control-plane classifier is required")
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(c.workspace, absolute)
	}
	absolute, err := filepath.Abs(absolute)
	if err != nil {
		return ControlPlaneClassification{}, false, err
	}
	relative, err := filepath.Rel(c.workspace, filepath.Clean(absolute))
	if err != nil {
		return ControlPlaneClassification{}, false, err
	}
	if outside(relative) {
		return ControlPlaneClassification{}, false, fmt.Errorf("path %q is outside workspace", path)
	}
	for _, component := range pathComponents(relative) {
		if name, protected := ControlPlaneName(component); protected {
			return ControlPlaneClassification{Root: name, Relative: relative}, true, nil
		}
	}
	return ControlPlaneClassification{Relative: relative}, false, nil
}

// CheckWrite rejects writes to protected metadata. Tree writes of the
// workspace root stay unbounded and are rejected; existing subdirectories
// are allowed as bounded write trees.
func (c *ControlPlane) CheckWrite(path string, tree bool) error {
	classification, protected, err := c.Classify(path)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf(
			"%w: %s (%s)",
			ErrProtectedPath,
			classification.Root,
			classification.Relative,
		)
	}
	if tree && (classification.Relative == "." || classification.Relative == "") {
		return fmt.Errorf(
			"%w: unbounded workspace tree write %s",
			ErrProtectedPath,
			classification.Relative,
		)
	}
	return nil
}

func pathComponents(relative string) []string {
	if relative == "." || relative == "" {
		return nil
	}
	return strings.Split(filepath.Clean(relative), string(filepath.Separator))
}

func outside(relative string) bool {
	return relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
