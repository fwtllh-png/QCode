package sandbox

import (
	"os"
	"path/filepath"
	"slices"
)

// ToolchainExposure describes immutable host installations that a sandbox may
// execute without exposing the host home or making installation state writable.
type ToolchainExposure struct {
	BinDirs     []string `json:"bin_dirs,omitempty"`
	ReadRoots   []string `json:"read_roots,omitempty"`
	ReadFiles   []string `json:"read_files,omitempty"`
	Environment []string `json:"environment,omitempty"`
}

func discoverToolchains(
	workspace string,
	runtimeRoots, existing []string,
) ToolchainExposure {
	searchDirs := executableSearchDirectories()
	seen := make(map[string]bool, len(runtimeRoots)+len(existing))
	for _, root := range append(append([]string(nil), runtimeRoots...), existing...) {
		seen[root] = true
	}
	var exposure ToolchainExposure
	for _, directory := range searchDirs {
		addToolchainDirectory(
			&exposure.BinDirs,
			directory,
			workspace,
			nil,
		)
		exposePATHExecutables(&exposure, directory, workspace, seen)
	}
	discoverPlatformToolchains(&exposure, workspace, seen)
	slices.Sort(exposure.ReadRoots)
	slices.Sort(exposure.ReadFiles)
	slices.Sort(exposure.Environment)
	return exposure
}

func executableSearchDirectories() []string {
	return PlatformPATHDirectories()
}

func addToolchainDirectory(
	target *[]string,
	path, workspace string,
	seen map[string]bool,
) {
	canonical, ok := canonicalToolchainDirectory(path, workspace)
	if !ok || (seen != nil && seen[canonical]) ||
		slices.Contains(*target, canonical) {
		return
	}
	if seen != nil {
		seen[canonical] = true
	}
	*target = append(*target, canonical)
}

func addToolchainReadDirectory(
	target *[]string,
	path, workspace string,
	seen map[string]bool,
) {
	lexical, canonical, err := canonicalHostReadRoot(path)
	if err != nil || validateInjectedRoot(canonical, workspace) != nil {
		return
	}
	for _, candidate := range []string{lexical, canonical} {
		if (seen != nil && seen[candidate]) ||
			slices.Contains(*target, candidate) {
			continue
		}
		if seen != nil {
			seen[candidate] = true
		}
		*target = append(*target, candidate)
	}
}

func addToolchainReadFile(target *[]string, path, workspace string) {
	lexical, canonical, err := canonicalHostReadFile(path)
	if err != nil || validateInjectedRoot(filepath.Dir(canonical), workspace) != nil {
		return
	}
	for _, candidate := range []string{lexical, canonical} {
		if !slices.Contains(*target, candidate) {
			*target = append(*target, candidate)
		}
	}
}

func canonicalToolchainDirectory(path, workspace string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	canonical, err := canonicalExisting(path)
	if err != nil || validateInjectedRoot(canonical, workspace) != nil {
		return "", false
	}
	return canonical, true
}
