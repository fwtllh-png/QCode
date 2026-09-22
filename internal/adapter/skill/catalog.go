package skill

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Catalog struct {
	mu             sync.RWMutex
	refreshMu      sync.Mutex
	discovery      DiscoveryOptions
	entries        map[string]candidate
	order          []string
	issues         []Issue
	locale         string
	limits         Limits
	state          *StateStore
	runtimeVersion string
	lock           *LockStore
	selectionMu    sync.Mutex
	selectionCache map[string]Selection
	selectionOrder []string
}

func Discover(options DiscoveryOptions) (*Catalog, error) {
	// Freeze discovery identities now; refresh must not follow a later cwd or
	// HOME change into a different workspace's private installation.
	var err error
	options.Workspace, err = filepath.Abs(options.Workspace)
	if err != nil {
		return nil, err
	}
	if options.UserHome == "" {
		options.UserHome, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	for _, path := range []*string{&options.UserHome, &options.SandboxHome} {
		if *path != "" {
			*path, err = filepath.Abs(*path)
			if err != nil {
				return nil, err
			}
		}
	}
	options.Limits = options.Limits.normalized()
	native, issues, err := discoverNative(options)
	if err != nil {
		return nil, err
	}
	var builtins []candidate
	if options.IncludeBuiltins {
		builtins, err = discoverBuiltins()
		if err != nil {
			return nil, err
		}
	}
	entries := make(map[string]candidate)
	var order []string
	for _, item := range append(native, builtins...) {
		if _, exists := entries[item.metadata.Name]; exists {
			continue
		}
		entries[item.metadata.Name] = item
		order = append(order, item.metadata.Name)
	}
	return &Catalog{
		discovery: options,
		entries:   entries, order: order, issues: append([]Issue(nil), issues...),
		locale: normalizeLocale(options.Locale), limits: options.Limits,
		state: options.State, runtimeVersion: normalizeRuntimeVersion(options.RuntimeVersion),
		lock:           options.Lock,
		selectionCache: make(map[string]Selection),
	}, nil
}

// Refresh replaces only the discovered inventory. State, lock and private-home
// authority remain bound to this catalog. Readers retain complete snapshots.
func (c *Catalog) Refresh(ctx context.Context) error {
	if c == nil {
		return errors.New("skill catalog is required")
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	next, err := Discover(c.discovery)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	changed := len(c.entries) != len(next.entries)
	for name, item := range next.entries {
		if current, exists := c.entries[name]; !exists ||
			current.digest != item.digest || current.path != item.path ||
			current.source != item.source {
			changed = true
			break
		}
	}
	c.entries, c.order, c.issues = next.entries, next.order, next.issues
	c.mu.Unlock()
	if changed {
		c.selectionMu.Lock()
		clear(c.selectionCache)
		c.selectionOrder = nil
		c.selectionMu.Unlock()
	}
	return nil
}

func (c *Catalog) frozen() *Catalog {
	entries, order, issues := c.snapshot()
	return &Catalog{
		entries: entries, order: order, issues: issues,
		locale: c.locale, limits: c.limits, state: c.state,
		runtimeVersion: c.runtimeVersion, lock: c.lock,
	}
}

func (c *Catalog) Issues() []Issue {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Issue(nil), c.issues...)
}

func (c *Catalog) Summaries(ctx context.Context) []Summary {
	summaries, _ := c.List(ctx)
	return summaries
}

func (c *Catalog) List(ctx context.Context) ([]Summary, []Issue) {
	if c == nil {
		return nil, nil
	}
	entries, order, baseIssues := c.snapshot()
	state, stateErr := c.stateSnapshot()
	issues := append([]Issue(nil), baseIssues...)
	if stateErr != nil {
		issues = append(issues, Issue{
			Path: c.state.Path(), Reason: stateErr.Error(),
		})
	}
	var result []Summary
	locked := c.lockEntries()
	for _, name := range order {
		item := entries[name]
		if !enabledFor(item, state, stateErr) {
			continue
		}
		result = append(result, c.summary(item, lockMatches(item, locked[name])))
	}
	if err := c.Verify(ctx); err != nil {
		path := "skill.lock.json"
		if c.lock != nil {
			path = c.lock.Path()
		}
		issues = append(issues, Issue{Path: path, Reason: err.Error()})
	}
	return result, issues
}

func (c *Catalog) Load(ctx context.Context, name string) (Loaded, error) {
	plan, err := c.LoadPlan(ctx, name)
	if err != nil {
		return Loaded{}, err
	}
	if len(plan) == 0 {
		return Loaded{}, fmt.Errorf("skill %q resolved to an empty plan", name)
	}
	return plan[len(plan)-1], nil
}

func (c *Catalog) SetEnabled(name string, enabled bool) error {
	if c == nil || c.state == nil {
		return errors.New("skill enable state store is not configured")
	}
	return c.state.SetEnabled(name, enabled)
}

func (c *Catalog) summary(item candidate, locked bool) Summary {
	version := legacySkillVersion
	compatibility := ""
	if item.manifest != nil {
		version = item.manifest.Version
		compatibility = item.manifest.QCode
	}
	return Summary{
		Name: item.metadata.Name, Description: item.metadata.DescriptionFor(c.locale),
		Source: item.source, Path: item.path,
		Version: version, Compatibility: compatibility,
		Digest: item.digest, Locked: locked,
		Handle: skillHandle(item), PackageHandle: skillPackageHandle(item),
		ResourceHandle: skillResourceHandle(item),
		ModelInvocable: !item.metadata.DisableModelInvocation,
	}
}

func (c *Catalog) stateSnapshot() (map[string]bool, error) {
	if c.state == nil {
		return map[string]bool{}, nil
	}
	return c.state.Snapshot()
}

func enabledFor(item candidate, state map[string]bool, stateErr error) bool {
	if stateErr != nil {
		return true
	}
	enabled, exists := state[item.metadata.Name]
	return !exists || enabled
}

func (c *Catalog) snapshot() (map[string]candidate, []string, []Issue) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := make(map[string]candidate, len(c.entries))
	maps.Copy(entries, c.entries)
	return entries, append([]string(nil), c.order...), append([]Issue(nil), c.issues...)
}

func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := append([]string(nil), c.order...)
	sort.Strings(result)
	return result
}
