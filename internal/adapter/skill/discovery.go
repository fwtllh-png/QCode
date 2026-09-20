package skill

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var workspaceSkillDirectories = []string{
	filepath.Join(".agents", "skills"),
	"skills",
	filepath.Join(".opencode", "skills"),
	filepath.Join(".claude", "skills"),
	filepath.Join(".cursor", "skills"),
	filepath.Join(".qcode", "skills"),
}

var userSkillDirectories = []string{
	filepath.Join(".agents", "skills"),
	filepath.Join(".claude", "skills"),
	filepath.Join(".qcode", "skills"),
}

type DiscoveryOptions struct {
	Workspace       string
	ConfiguredDir   string
	UserHome        string
	SandboxHome     string
	Locale          string
	IncludeBuiltins bool
	Limits          Limits
	State           *StateStore
	RuntimeVersion  string
	Lock            *LockStore
}

type rootSpec struct {
	path     string
	source   Source
	boundary string
}

type candidate struct {
	metadata    Metadata
	source      Source
	root        string
	relative    string
	path        string
	manifest    *Manifest
	digest      string
	rawSkill    []byte
	rawManifest []byte
}

func discoverNative(options DiscoveryOptions) ([]candidate, []Issue, error) {
	limits := options.Limits.normalized()
	workspace, err := secureDirectory(options.Workspace, true)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve skill workspace: %w", err)
	}
	roots := make([]rootSpec, 0, len(workspaceSkillDirectories)+4)
	for _, relative := range workspaceSkillDirectories {
		roots = append(roots, rootSpec{
			path: filepath.Join(workspace, relative), source: SourceWorkspace,
		})
	}
	if strings.TrimSpace(options.ConfiguredDir) != "" {
		configured := options.ConfiguredDir
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(workspace, configured)
		}
		roots = append(roots, rootSpec{path: filepath.Clean(configured), source: SourceConfigured})
	}
	if options.SandboxHome != "" {
		for _, relative := range userSkillDirectories {
			roots = append(roots, rootSpec{
				path: filepath.Join(options.SandboxHome, relative), source: SourceWorkspace,
				boundary: options.SandboxHome,
			})
		}
	}
	home := options.UserHome
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		for _, relative := range userSkillDirectories {
			roots = append(roots, rootSpec{
				path: filepath.Join(home, relative), source: SourceUser,
			})
		}
	}

	seen := make(map[string]struct{})
	var result []candidate
	var issues []Issue
	visited := 0
	scanned := 0
	for _, root := range roots {
		items, rootIssues, walkErr := walkSkillRoot(root, limits, &visited, &scanned)
		issues = append(issues, rootIssues...)
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				continue
			}
			if root.source == SourceConfigured {
				return nil, issues, fmt.Errorf("discover configured skill directory %q: %w", root.path, walkErr)
			}
			issues = append(issues, Issue{Path: root.path, Reason: walkErr.Error()})
			continue
		}
		if root.source == SourceConfigured && len(rootIssues) != 0 {
			return nil, issues, fmt.Errorf(
				"discover configured skill %q: %s",
				rootIssues[0].Path, rootIssues[0].Reason,
			)
		}
		for _, item := range items {
			if _, duplicate := seen[item.metadata.Name]; duplicate {
				continue
			}
			seen[item.metadata.Name] = struct{}{}
			result = append(result, item)
		}
		if visited >= limits.MaxSkills {
			issues = append(issues, Issue{
				Path: root.path, Reason: "skill discovery entry limit reached",
			})
			break
		}
	}
	return result, issues, nil
}

func walkSkillRoot(
	spec rootSpec,
	limits Limits,
	visited *int,
	scanned *int,
) ([]candidate, []Issue, error) {
	root, err := secureDirectory(spec.path, false)
	if err != nil {
		return nil, nil, err
	}
	if spec.boundary != "" {
		boundary, boundaryErr := secureDirectory(spec.boundary, true)
		if boundaryErr != nil {
			return nil, nil, boundaryErr
		}
		relative, relErr := filepath.Rel(boundary, root)
		if relErr != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return nil, nil, errors.New("skill directory escapes sandbox home")
		}
	}
	var result []candidate
	var issues []Issue
	var walk func(string, int)
	walk = func(relative string, depth int) {
		if depth > limits.MaxDepth || *visited >= limits.MaxSkills {
			return
		}
		directoryPath := filepath.Join(root, relative)
		remaining := limits.MaxEntries - *scanned
		if remaining <= 0 {
			issues = append(issues, Issue{
				Path: directoryPath, Reason: "skill discovery entry limit reached",
			})
			return
		}
		directory, openErr := os.Open(directoryPath)
		if openErr != nil {
			issues = append(issues, Issue{Path: directoryPath, Reason: openErr.Error()})
			return
		}
		entries, readErr := directory.ReadDir(remaining + 1)
		closeErr := directory.Close()
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		readErr = errors.Join(readErr, closeErr)
		if readErr != nil {
			issues = append(issues, Issue{Path: directoryPath, Reason: readErr.Error()})
			return
		}
		if len(entries) > remaining {
			*scanned = limits.MaxEntries
			issues = append(issues, Issue{
				Path: directoryPath, Reason: "skill discovery entry limit reached",
			})
			return
		}
		*scanned += len(entries)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if *visited >= limits.MaxSkills {
				return
			}
			childRelative := filepath.Join(relative, entry.Name())
			childPath := filepath.Join(root, childRelative)
			info, statErr := os.Lstat(childPath)
			if statErr != nil {
				issues = append(issues, Issue{Path: childPath, Reason: statErr.Error()})
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				issues = append(issues, Issue{Path: childPath, Reason: "symlink skill path rejected"})
				continue
			}
			if info.IsDir() {
				walk(childRelative, depth+1)
				continue
			}
			if entry.Name() != "SKILL.md" {
				continue
			}
			(*visited)++
			data, readErr := readRegularAt(root, childRelative, limits.MaxFileBytes)
			if readErr != nil {
				issues = append(issues, Issue{Path: childPath, Reason: readErr.Error()})
				continue
			}
			document, parseErr := parseDocument(data)
			if parseErr != nil {
				issues = append(issues, Issue{Path: childPath, Reason: parseErr.Error()})
				continue
			}
			manifestRelative := filepath.Join(filepath.Dir(childRelative), ManifestFileName)
			manifestData, manifestErr := readRegularAt(root, manifestRelative, 64<<10)
			var manifest *Manifest
			switch {
			case manifestErr == nil:
				parsed, parseManifestErr := ParseManifest(manifestData)
				if parseManifestErr != nil {
					issues = append(issues, Issue{
						Path:   filepath.Join(root, manifestRelative),
						Reason: parseManifestErr.Error(),
					})
					continue
				}
				if parsed.Name != document.metadata.Name {
					issues = append(issues, Issue{
						Path:   childPath,
						Reason: "skill.toml and SKILL.md names do not match",
					})
					continue
				}
				manifest = &parsed
			case errors.Is(manifestErr, os.ErrNotExist):
				if spec.source == SourceConfigured {
					issues = append(issues, Issue{
						Path:   filepath.Join(root, manifestRelative),
						Reason: "governed skill requires skill.toml",
					})
					continue
				}
			default:
				issues = append(issues, Issue{
					Path: filepath.Join(root, manifestRelative), Reason: manifestErr.Error(),
				})
				continue
			}
			result = append(result, candidate{
				metadata: document.metadata, source: spec.source,
				root: root, relative: childRelative, path: childPath,
				manifest: manifest, digest: skillDigest(document.raw, manifestData),
			})
		}
	}
	walk("", 0)
	return result, issues, nil
}

func secureDirectory(path string, required bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("skill directory symlink rejected")
	}
	if !info.IsDir() {
		return "", errors.New("skill root must be a directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func readRegularAt(root, relative string, maxBytes int64) ([]byte, error) {
	if filepath.IsAbs(relative) {
		return nil, errors.New("absolute skill path rejected")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("skill path escapes root")
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlink skill path rejected")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, errors.New("skill parent is not a directory")
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, errors.New("skill path is not a regular file")
		}
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxBytes {
		return nil, errors.New("skill file exceeds size limit or is not regular")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("skill file exceeds size limit")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(current)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) ||
		!os.SameFile(after, pathInfo) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("skill file changed while reading")
	}
	return data, nil
}
