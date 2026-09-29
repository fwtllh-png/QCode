//go:build darwin

package sandbox

import (
	"debug/macho"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type machoRuntimeMetadata struct {
	libraries []string
	rpaths    []string
}

// Bind only files referenced by the executable's load commands. Adjacent
// configuration, package data and writable state require explicit declarations.
func executableRuntimeDependencies(executable string) []string {
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil
	}
	executableDir := filepath.Dir(executable)
	pending := []string{executable}
	visited := make(map[string]bool)
	var files []string
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if visited[current] {
			continue
		}
		visited[current] = true

		metadata, err := readMachORuntimeMetadata(current)
		if err != nil {
			continue
		}
		for _, library := range metadata.libraries {
			dependency := resolveMachOLibrary(
				library,
				current,
				executableDir,
				metadata.rpaths,
			)
			if dependency == "" {
				continue
			}
			info, err := os.Stat(dependency)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			canonical, err := filepath.EvalSymlinks(dependency)
			if err != nil {
				continue
			}
			if validateSensitivePath(dependency) != nil || validateSensitivePath(canonical) != nil {
				continue
			}
			if _, err := readMachORuntimeMetadata(canonical); err != nil {
				continue
			}
			for _, path := range dependencyReadPaths(dependency, canonical) {
				if !slices.Contains(files, path) {
					files = append(files, path)
				}
			}
			if !visited[canonical] {
				pending = append(pending, canonical)
			}
		}
	}
	return files
}

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

func readMachORuntimeMetadata(path string) (machoRuntimeMetadata, error) {
	file, err := macho.Open(path)
	if err == nil {
		defer file.Close()
		return metadataFromMachOFiles(file)
	}
	fat, fatErr := macho.OpenFat(path)
	if fatErr != nil {
		return machoRuntimeMetadata{}, fatErr
	}
	defer fat.Close()
	files := make([]*macho.File, 0, len(fat.Arches))
	for _, arch := range fat.Arches {
		files = append(files, arch.File)
	}
	return metadataFromMachOFiles(files...)
}

func metadataFromMachOFiles(files ...*macho.File) (machoRuntimeMetadata, error) {
	var metadata machoRuntimeMetadata
	for _, file := range files {
		libraries, err := file.ImportedLibraries()
		if err != nil {
			return machoRuntimeMetadata{}, err
		}
		for _, library := range libraries {
			if !slices.Contains(metadata.libraries, library) {
				metadata.libraries = append(metadata.libraries, library)
			}
		}
		for _, load := range file.Loads {
			rpath, ok := load.(*macho.Rpath)
			if ok && !slices.Contains(metadata.rpaths, rpath.Path) {
				metadata.rpaths = append(metadata.rpaths, rpath.Path)
			}
		}
	}
	return metadata, nil
}

func resolveMachOLibrary(
	library, loader, executableDir string,
	rpaths []string,
) string {
	switch {
	case filepath.IsAbs(library):
		return existingFile(library)
	case strings.HasPrefix(library, "@loader_path/"):
		return existingFile(filepath.Join(
			filepath.Dir(loader),
			strings.TrimPrefix(library, "@loader_path/"),
		))
	case strings.HasPrefix(library, "@executable_path/"):
		return existingFile(filepath.Join(
			executableDir,
			strings.TrimPrefix(library, "@executable_path/"),
		))
	case strings.HasPrefix(library, "@rpath/"):
		suffix := strings.TrimPrefix(library, "@rpath/")
		for _, rpath := range rpaths {
			base := resolveMachORPath(rpath, loader, executableDir)
			if base == "" {
				continue
			}
			if dependency := existingFile(filepath.Join(base, suffix)); dependency != "" {
				return dependency
			}
		}
	}
	return ""
}

func resolveMachORPath(rpath, loader, executableDir string) string {
	switch {
	case rpath == "@loader_path":
		return filepath.Dir(loader)
	case strings.HasPrefix(rpath, "@loader_path/"):
		return filepath.Join(
			filepath.Dir(loader),
			strings.TrimPrefix(rpath, "@loader_path/"),
		)
	case rpath == "@executable_path":
		return executableDir
	case strings.HasPrefix(rpath, "@executable_path/"):
		return filepath.Join(
			executableDir,
			strings.TrimPrefix(rpath, "@executable_path/"),
		)
	case filepath.IsAbs(rpath):
		return rpath
	default:
		return ""
	}
}

func existingFile(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}
