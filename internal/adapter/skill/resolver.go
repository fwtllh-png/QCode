package skill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ResolvedSkill struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Source       Source            `json:"source"`
	Path         string            `json:"path"`
	Digest       string            `json:"digest"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Locked       bool              `json:"locked"`
}

func (c *Catalog) Resolve(ctx context.Context) ([]ResolvedSkill, error) {
	items, err := c.resolveInventory(ctx)
	if err != nil {
		return nil, err
	}
	return resolvedSkills(items, false), nil
}

func (c *Catalog) WriteLock(ctx context.Context) (Lockfile, error) {
	if c == nil || c.lock == nil {
		return Lockfile{}, fmt.Errorf("%w: lock store is not configured", ErrLockDrift)
	}
	items, err := c.resolveInventory(ctx)
	if err != nil {
		return Lockfile{}, err
	}
	for _, item := range items {
		if _, err := c.loadCandidate(ctx, item, false); err != nil {
			return Lockfile{}, err
		}
	}
	lockfile := lockfileFor(c.runtimeVersion, items)
	if err := c.lock.Write(lockfile); err != nil {
		return Lockfile{}, err
	}
	return lockfile, nil
}

func (c *Catalog) Verify(ctx context.Context) error {
	if c == nil {
		return errors.New("skill catalog is required")
	}
	items, err := c.resolveInventory(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err := c.loadCandidate(ctx, item, false); err != nil {
			return err
		}
	}
	if c.lock == nil {
		if len(items) == 0 {
			return nil
		}
		return fmt.Errorf("%w: governed skills require a lock store", ErrLockDrift)
	}
	expected := lockfileFor(c.runtimeVersion, items)
	lockfile, err := c.lock.Read()
	if err != nil {
		if len(expected.Skills) == 0 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: read skill lock: %v", ErrLockDrift, err)
	}
	if len(expected.Skills) == 0 && len(lockfile.Skills) == 0 {
		return nil
	}
	if err := compareLockfiles(expected, lockfile); err != nil {
		return fmt.Errorf("%w: %v", ErrLockDrift, err)
	}
	return nil
}

func (c *Catalog) LoadPlan(ctx context.Context, name string) ([]Loaded, error) {
	if c == nil {
		return nil, errors.New("skill catalog is required")
	}
	if !namePattern.MatchString(name) {
		return nil, errors.New("skill name is invalid")
	}
	items, err := c.resolveRoots(ctx, []string{name}, false)
	if err != nil {
		return nil, err
	}
	governed := false
	for _, item := range items {
		governed = governed || requiresWorkspaceLock(item)
	}
	if governed {
		if err := c.Verify(ctx); err != nil {
			return nil, err
		}
	}
	var total int64
	result := make([]Loaded, 0, len(items))
	for _, item := range items {
		loaded, err := c.loadCandidate(ctx, item, item.manifest != nil)
		if err != nil {
			return nil, err
		}
		total += int64(len(loaded.Content))
		if total > c.limits.MaxLoadBytes {
			return nil, errors.New("resolved skill instructions exceed load byte limit")
		}
		result = append(result, loaded)
	}
	return result, nil
}

// resolveInventory locks discovered packages and their dependencies independently
// of enablement. LoadPlan still checks enablement for every requested dependency.
func (c *Catalog) resolveInventory(ctx context.Context) ([]candidate, error) {
	entries, order, _ := c.snapshot()
	var roots []string
	for _, name := range order {
		item := entries[name]
		if requiresWorkspaceLock(item) {
			roots = append(roots, name)
		}
	}
	return c.resolveEntries(ctx, entries, nil, roots, true)
}

func (c *Catalog) resolveRoots(
	ctx context.Context,
	roots []string,
	governedOnly bool,
) ([]candidate, error) {
	entries, _, _ := c.snapshot()
	state, stateErr := c.stateSnapshot()
	if stateErr != nil {
		return nil, stateErr
	}
	return c.resolveEntries(ctx, entries, state, roots, governedOnly)
}

func (c *Catalog) resolveEntries(
	ctx context.Context,
	entries map[string]candidate,
	state map[string]bool,
	roots []string,
	governedOnly bool,
) ([]candidate, error) {
	const (
		visiting = 1
		visited  = 2
	)
	colors := make(map[string]int)
	var stack []string
	var result []candidate
	var visit func(string, string) error
	visit = func(name, requiredBy string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if colors[name] == visiting {
			start := 0
			for index, current := range stack {
				if current == name {
					start = index
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), name)
			return fmt.Errorf("%w: %s", ErrDependencyCycle, strings.Join(cycle, " -> "))
		}
		if colors[name] == visited {
			return nil
		}
		item, exists := entries[name]
		if !exists {
			return fmt.Errorf(
				"%w: dependency %q required by %q was not found",
				ErrDependencyConflict, name, requiredBy,
			)
		}
		if !enabledFor(item, state, nil) {
			return fmt.Errorf(
				"%w: dependency %q required by %q is disabled",
				ErrDependencyConflict, name, requiredBy,
			)
		}
		if item.manifest == nil {
			if governedOnly || requiredBy != "" {
				return fmt.Errorf(
					"%w: skill %q is local/unlocked and cannot satisfy dependencies",
					ErrDependencyConflict, name,
				)
			}
			colors[name] = visited
			result = append(result, item)
			return nil
		}
		if err := checkVersion(item.manifest.QCode, c.runtimeVersion); err != nil {
			return fmt.Errorf(
				"%w: skill %q with QCode %s: %v",
				ErrCompatibilityMismatch, name, c.runtimeVersion, err,
			)
		}
		if len(result)+len(stack) >= c.limits.MaxResolved {
			return fmt.Errorf("%w: resolution limit exceeded", ErrDependencyConflict)
		}
		colors[name] = visiting
		stack = append(stack, name)
		for _, dependency := range sortedDependencyNames(item.manifest.Dependencies) {
			dependencyItem, exists := entries[dependency]
			if !exists || dependencyItem.manifest == nil {
				return fmt.Errorf(
					"%w: skill %q dependency %q has no governed version",
					ErrDependencyConflict, name, dependency,
				)
			}
			constraint := item.manifest.Dependencies[dependency]
			if err := checkVersion(constraint, dependencyItem.manifest.Version); err != nil {
				return fmt.Errorf(
					"%w: skill %q dependency %q@%s does not satisfy %s",
					ErrDependencyConflict, name, dependency,
					dependencyItem.manifest.Version, constraint,
				)
			}
			if err := visit(dependency, name); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		colors[name] = visited
		result = append(result, item)
		return nil
	}
	for _, root := range roots {
		if err := visit(root, ""); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c *Catalog) loadCandidate(
	ctx context.Context,
	item candidate,
	locked bool,
) (Loaded, error) {
	rawSkill := append([]byte(nil), item.rawSkill...)
	rawManifest := append([]byte(nil), item.rawManifest...)
	if item.source != SourceBuiltin {
		var err error
		rawSkill, err = readRegularAt(item.root, item.relative, c.limits.MaxFileBytes)
		if err != nil {
			return Loaded{}, fmt.Errorf("read skill safely: %w", err)
		}
		if item.manifest != nil {
			manifestRelative := filepath.Join(filepath.Dir(item.relative), ManifestFileName)
			rawManifest, err = readRegularAt(item.root, manifestRelative, 64<<10)
			if err != nil {
				return Loaded{}, fmt.Errorf("read skill manifest safely: %w", err)
			}
		}
	}
	document, err := parseDocument(rawSkill)
	if err != nil {
		return Loaded{}, fmt.Errorf("revalidate skill: %w", err)
	}
	if document.metadata.Name != item.metadata.Name {
		return Loaded{}, errors.New("skill identity changed after discovery")
	}
	if digest := skillDigest(rawSkill, rawManifest); digest != item.digest {
		return Loaded{}, fmt.Errorf(
			"%w: skill %q digest drifted after discovery", ErrLockDrift, item.metadata.Name,
		)
	}
	if item.manifest != nil {
		manifest, err := ParseManifest(rawManifest)
		if err != nil {
			return Loaded{}, fmt.Errorf("revalidate skill manifest: %w", err)
		}
		if manifest.Name != item.manifest.Name || manifest.Version != item.manifest.Version {
			return Loaded{}, errors.New("skill manifest identity changed after discovery")
		}
	}
	summary := c.summary(item, locked)
	summary.Description = document.metadata.DescriptionFor(c.locale)
	var dependencies map[string]string
	if item.manifest != nil {
		dependencies = cloneDependencies(item.manifest.Dependencies)
	}
	return Loaded{
		Summary: summary, Content: document.body, Dependencies: dependencies,
	}, nil
}

func requiresWorkspaceLock(item candidate) bool {
	return item.manifest != nil && item.source != SourceBuiltin
}

func resolvedSkills(items []candidate, locked bool) []ResolvedSkill {
	result := make([]ResolvedSkill, 0, len(items))
	for _, item := range items {
		version := legacySkillVersion
		var dependencies map[string]string
		if item.manifest != nil {
			version = item.manifest.Version
			dependencies = cloneDependencies(item.manifest.Dependencies)
		}
		result = append(result, ResolvedSkill{
			Name: item.metadata.Name, Version: version, Source: item.source,
			Path: item.path, Digest: item.digest, Dependencies: dependencies,
			Locked: item.source == SourceBuiltin || locked && item.manifest != nil,
		})
	}
	return result
}

func lockfileFor(runtimeVersion string, items []candidate) Lockfile {
	lockfile := Lockfile{
		SchemaVersion:  LockSchemaV1,
		RuntimeVersion: normalizeRuntimeVersion(runtimeVersion),
		Skills:         make([]LockEntry, 0, len(items)),
	}
	for _, item := range items {
		if item.manifest == nil {
			continue
		}
		lockfile.Skills = append(lockfile.Skills, LockEntry{
			Name: item.metadata.Name, Version: item.manifest.Version,
			Source: item.source, Digest: item.digest,
			Dependencies: cloneDependencies(item.manifest.Dependencies),
		})
	}
	sort.Slice(lockfile.Skills, func(i, j int) bool {
		return lockfile.Skills[i].Name < lockfile.Skills[j].Name
	})
	return lockfile
}

func compareLockfiles(expected, actual Lockfile) error {
	if expected.SchemaVersion != actual.SchemaVersion ||
		expected.RuntimeVersion != actual.RuntimeVersion {
		return errors.New("lock header differs from current runtime")
	}
	if len(expected.Skills) != len(actual.Skills) {
		return fmt.Errorf(
			"locked skill count %d differs from resolved count %d",
			len(actual.Skills), len(expected.Skills),
		)
	}
	actualByName := make(map[string]LockEntry, len(actual.Skills))
	for _, entry := range actual.Skills {
		actualByName[entry.Name] = entry
	}
	for _, wanted := range expected.Skills {
		got, ok := actualByName[wanted.Name]
		if !ok {
			return fmt.Errorf("skill %q is missing from lock", wanted.Name)
		}
		if wanted.Version != got.Version || wanted.Source != got.Source ||
			wanted.Digest != got.Digest ||
			!equalDependencies(wanted.Dependencies, got.Dependencies) {
			return fmt.Errorf("skill %q differs from lock", wanted.Name)
		}
	}
	return nil
}

func equalDependencies(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, constraint := range left {
		if right[name] != constraint {
			return false
		}
	}
	return true
}

func (c *Catalog) lockEntries() map[string]LockEntry {
	if c == nil || c.lock == nil {
		return nil
	}
	lockfile, err := c.lock.Read()
	if err != nil || lockfile.RuntimeVersion != normalizeRuntimeVersion(c.runtimeVersion) {
		return nil
	}
	result := make(map[string]LockEntry, len(lockfile.Skills))
	for _, entry := range lockfile.Skills {
		result[entry.Name] = entry
	}
	return result
}

func lockMatches(item candidate, entry LockEntry) bool {
	if item.source == SourceBuiltin {
		return true
	}
	return item.manifest != nil &&
		entry.Name == item.metadata.Name &&
		entry.Version == item.manifest.Version &&
		entry.Source == item.source &&
		entry.Digest == item.digest &&
		equalDependencies(entry.Dependencies, item.manifest.Dependencies)
}
