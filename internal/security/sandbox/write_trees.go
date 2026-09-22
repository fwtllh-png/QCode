package sandbox

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/controlplane"
)

// CollectWriteTreeFiles lists regular files under an existing workspace write
// tree. Symlinks are not followed. Protected control-plane directories are
// skipped. The walk fails if it would exceed limit.
func CollectWriteTreeFiles(root, tree string, limit int) ([]string, error) {
	if limit < 0 {
		return nil, fmt.Errorf("write tree %q exceeds the %d-file limit", tree, MaxExactWorkspaceWritePaths)
	}
	classifier, err := controlplane.New(root)
	if err != nil {
		return nil, err
	}
	absolute := tree
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(classifier.Workspace(), absolute)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	relative, err := filepath.Rel(classifier.Workspace(), absolute)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("write tree %q is outside workspace", tree)
	}
	var files []string
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := classifier.CheckWrite(path, entry.IsDir()); err != nil {
			if entry.IsDir() && path != absolute {
				return filepath.SkipDir
			}
			if path == absolute {
				return err
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		if len(files) >= limit {
			return fmt.Errorf(
				"write tree %q exceeds the %d-file limit",
				tree,
				MaxExactWorkspaceWritePaths,
			)
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
