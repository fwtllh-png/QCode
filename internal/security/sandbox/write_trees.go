package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	securitypaths "github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

// CollectWriteTreeFiles lists regular files under an existing workspace write
// tree. Symlinks are not followed. Protected control-plane directories are
// skipped. Directory contents do not consume the declared-path allowance.
// Cancellation bounds the walk using the caller's execution lifetime.
func CollectWriteTreeFiles(ctx context.Context, root, tree string) ([]string, error) {
	classifier, err := securitypaths.NewControlPlane(root)
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
