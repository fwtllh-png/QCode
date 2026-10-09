package sandbox

import (
	"os"
	"path/filepath"
	"slices"
)

// dyld can check a path after resolving its directory but before resolving the
// final library symlink. Include those exact aliases as well as the real file.
func dependencyReadPaths(path, canonical string) []string {
	paths := []string{path, canonical}
	for {
		directory, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return paths
		}
		path = filepath.Join(directory, filepath.Base(path))
		if slices.Contains(paths[1:], path) {
			return paths
		}
		paths = append(paths, path)
		target, err := os.Readlink(path)
		if err != nil {
			return paths
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(directory, target)
		}
		path = target
	}
}
