// Package controlplane protects runtime and agent metadata from workload writes.
package controlplane

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

var ErrProtected = errors.New("security control-plane path is protected")

var protectedNames = map[string]struct{}{
	".agents":         {},
	".qcode":          {},
	".qcode-worktree": {},
	".codex":          {},
	".git":            {},
}

type Classification struct {
	Root     string
	Relative string
}

type Classifier struct {
	workspace string
}

func New(workspace string) (*Classifier, error) {
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
	return &Classifier{workspace: filepath.Clean(resolved)}, nil
}

func (c *Classifier) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

func (c *Classifier) Classify(path string) (Classification, bool, error) {
	if c == nil {
		return Classification{}, false, errors.New("control-plane classifier is required")
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(c.workspace, absolute)
	}
	absolute, err := filepath.Abs(absolute)
	if err != nil {
		return Classification{}, false, err
	}
	relative, err := filepath.Rel(c.workspace, filepath.Clean(absolute))
	if err != nil {
		return Classification{}, false, err
	}
	if outside(relative) {
		return Classification{}, false, fmt.Errorf("path %q is outside workspace", path)
	}
	for _, component := range pathComponents(relative) {
		name := strings.ToLower(component)
		if _, protected := protectedNames[name]; protected {
			return Classification{Root: name, Relative: relative}, true, nil
		}
	}
	return Classification{Relative: relative}, false, nil
}

// CheckWrite rejects writes to protected metadata. Tree writes of the
// workspace root stay unbounded and are rejected; existing subdirectories
// are allowed as bounded write trees.
func (c *Classifier) CheckWrite(path string, tree bool) error {
	classification, protected, err := c.Classify(path)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf(
			"%w: %s (%s)",
			ErrProtected,
			classification.Root,
			classification.Relative,
		)
	}
	if tree && (classification.Relative == "." || classification.Relative == "") {
		return fmt.Errorf(
			"%w: unbounded workspace tree write %s",
			ErrProtected,
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

// ProtectedNames returns the control-plane entry names rejected for
// workload writes, sorted. The sandbox profile denies them inside approved
// write trees so the OS-level grant is never broader than the settlement
// classifier that approved the tree.
func ProtectedNames() []string {
	names := make([]string, 0, len(protectedNames))
	for name := range protectedNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
