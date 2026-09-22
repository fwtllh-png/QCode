package sandbox

import (
	"os"
	"path/filepath"
	"slices"
)

// PlatformPATHDirectories returns absolute PATH directories from the process
// environment plus platform path sources. It does not scan Home.
func PlatformPATHDirectories() []string {
	var result []string
	for _, directory := range append(
		filepath.SplitList(os.Getenv("PATH")),
		extraPlatformPATHDirectories()...,
	) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			continue
		}
		canonical, err := filepath.EvalSymlinks(directory)
		if err != nil {
			continue
		}
		canonical = filepath.Clean(canonical)
		if !slices.Contains(result, canonical) {
			result = append(result, canonical)
		}
	}
	return result
}

func exposePATHExecutables(
	exposure *ToolchainExposure,
	directory, workspace string,
	seen map[string]bool,
) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		if resolved == filepath.Clean(path) {
			continue
		}
		bin := filepath.Dir(resolved)
		if bin == filepath.Clean(directory) {
			continue
		}
		addToolchainReadDirectory(&exposure.ReadRoots, bin, workspace, seen)
		if filepath.Base(bin) == "bin" {
			addToolchainReadDirectory(
				&exposure.ReadRoots,
				filepath.Dir(bin),
				workspace,
				seen,
			)
		}
	}
}
