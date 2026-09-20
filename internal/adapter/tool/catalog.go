package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	catalogSequence atomic.Uint64
	legacySequence  atomic.Uint64
)

const (
	ErrorCategoryUnknownTool      = "unknown_tool"
	ErrorCategoryToolUnavailable  = "tool_unavailable"
	ErrorCategoryInvalidArguments = "invalid_arguments"
	ErrorCategoryPrecondition     = "tool_precondition"
	ErrorCategoryToolCatalogStale = "tool_catalog_stale"
	ErrorCategoryToolRevoked      = "tool_revoked"
	ErrorCategoryToolLoadFailed   = "tool_load_failed"
	ErrorCategoryToolCatalogLimit = "tool_catalog_limit"

	DefaultMaxMaterialized            = 32
	DefaultMaxMaterializedSchemaBytes = 64 << 10
)

// ErrorCategory returns a stable catalog failure category.
func ErrorCategory(err error) string {
	switch {
	case errors.Is(err, ErrUnknownTool):
		return ErrorCategoryUnknownTool
	case errors.Is(err, ErrToolUnavailable):
		return ErrorCategoryToolUnavailable
	case errors.Is(err, ErrInvalidArguments):
		return ErrorCategoryInvalidArguments
	case errors.Is(err, ErrPrecondition):
		return ErrorCategoryPrecondition
	case errors.Is(err, ErrCatalogStale):
		return ErrorCategoryToolCatalogStale
	case errors.Is(err, ErrToolRevoked):
		return ErrorCategoryToolRevoked
	case errors.Is(err, ErrToolLoadFailed):
		return ErrorCategoryToolLoadFailed
	case errors.Is(err, ErrCatalogLimit):
		return ErrorCategoryToolCatalogLimit
	default:
		return ""
	}
}

type CatalogBinding struct {
	CatalogID  string
	Generation uint64
	Revision   uint64
	// Authority is the Registry-private entry incarnation. It is intentionally
	// absent from host/provider wire formats and changes on every replacement.
	Authority uint64
}

func (s CatalogSnapshot) Binding(name string) (CatalogBinding, bool) {
	entry, ok := s.Lookup(name)
	if !ok {
		return CatalogBinding{}, false
	}
	return CatalogBinding{
		CatalogID: s.CatalogID, Generation: s.Generation,
		Revision: entry.Revision, Authority: entry.authority,
	}, true
}

func (r *Registry) SetMaterializeLimits(maxEntries, maxSchemaBytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxEntries <= 0 {
		maxEntries = DefaultMaxMaterialized
	}
	if maxSchemaBytes <= 0 {
		maxSchemaBytes = DefaultMaxMaterializedSchemaBytes
	}
	r.maxMaterialized = maxEntries
	r.maxSchemaBytes = maxSchemaBytes
}

// Materialize loads or pins an entry for subsequent samples.
func (r *Registry) Materialize(name string, expectedRevision uint64) (CatalogChange, error) {
	r.mu.Lock()
	if canonical := r.aliases[name]; canonical != "" {
		name = canonical
	}
	item := r.tools[name]
	if item == nil {
		r.mu.Unlock()
		if _, revoked := r.tombstones[name]; revoked {
			return CatalogChange{}, fmt.Errorf("%w %q", ErrToolRevoked, name)
		}
		return CatalogChange{}, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}
	if expectedRevision != 0 && item.revision != expectedRevision {
		r.mu.Unlock()
		return CatalogChange{}, fmt.Errorf(
			"%w for tool %q: expected revision=%d current=%d",
			ErrCatalogStale, name, expectedRevision, item.revision,
		)
	}
	if item.state == CatalogEntryMaterialized {
		change := CatalogChange{Name: name, Source: item.source, Revision: item.revision}
		r.mu.Unlock()
		return change, nil
	}
	if item.deferred == nil {
		if err := r.checkMaterializeLimitLocked(item); err != nil {
			r.mu.Unlock()
			return CatalogChange{}, err
		}
		item.state = CatalogEntryMaterialized
		r.generation++
		change := CatalogChange{Name: name, Source: item.source, Revision: item.revision}
		r.mu.Unlock()
		return change, nil
	}
	r.mu.Unlock()

	_, _, _, err := r.Resolve(name)
	if err != nil {
		if !errors.Is(err, ErrCatalogLimit) {
			return CatalogChange{}, fmt.Errorf("%w: %v", ErrToolLoadFailed, err)
		}
		return CatalogChange{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.tools[name]
	if current == nil {
		return CatalogChange{}, fmt.Errorf("%w %q", ErrToolRevoked, name)
	}
	if current != item {
		return CatalogChange{}, fmt.Errorf("%w for tool %q", ErrCatalogStale, name)
	}
	if expectedRevision != 0 && current.revision < expectedRevision {
		return CatalogChange{}, fmt.Errorf("%w for tool %q", ErrCatalogStale, name)
	}
	if current.state != CatalogEntryMaterialized {
		current.state = CatalogEntryMaterialized
		current.revision++
		r.generation++
	}
	return CatalogChange{Name: name, Source: current.source, Revision: current.revision}, nil
}

func (r *Registry) checkMaterializeLimitLocked(candidate *registered) error {
	count, schemaBytes := 0, 0
	for _, item := range r.tools {
		if item.state != CatalogEntryMaterialized && !item.loading {
			continue
		}
		count++
		data, _ := json.Marshal(item.descriptor.InputSchema)
		schemaBytes += len(data)
	}
	data, _ := json.Marshal(candidate.descriptor.InputSchema)
	if count+1 > r.maxMaterialized || schemaBytes+len(data) > r.maxSchemaBytes {
		return fmt.Errorf(
			"%w: entries=%d/%d schema_bytes=%d/%d",
			ErrCatalogLimit, count+1, r.maxMaterialized,
			schemaBytes+len(data), r.maxSchemaBytes,
		)
	}
	return nil
}

// Registration is one source-owned desired entry.
type Registration struct {
	descriptor Descriptor
	external   ExternalDescriptor
	binding    TrustedBinding
	explicit   bool
	executor   Executor
	deferred   func() (Executor, error)
	state      CatalogEntryState
	payload    any
	token      uint64
}

func NewRegistration(executor Executor) Registration {
	return Registration{executor: executor}
}

func NewExternalRegistration(
	external ExternalDescriptor,
	binding TrustedBinding,
	executor Executor,
) Registration {
	return Registration{
		external: external, binding: binding, executor: executor, explicit: true,
	}
}

func NewExternalDeferredRegistration(
	external ExternalDescriptor,
	binding TrustedBinding,
	loader func() (Executor, error),
) Registration {
	external.Deferred.Enabled = true
	external.Availability = AvailabilityDeferred
	return Registration{
		external: external, binding: binding, deferred: loader,
		state: CatalogEntryDeferred, explicit: true,
	}
}

func (r Registration) WithPayload(payload any) Registration {
	r.payload = payload
	return r
}

func (r Registration) Descriptor() Descriptor {
	if r.descriptor.Name == "" && r.executor != nil {
		return cloneDescriptor(r.executor.Descriptor())
	}
	return cloneDescriptor(r.descriptor)
}
func (r Registration) TrustedBinding() TrustedBinding {
	if r.explicit {
		return cloneTrustedBinding(r.binding)
	}
	if provider, ok := r.executor.(TrustedBindingProvider); ok {
		return cloneTrustedBinding(provider.TrustedBinding())
	}
	return TrustedBindingFromDescriptor(r.Descriptor())
}
func (r Registration) Executor() Executor { return r.executor }
func (r Registration) Payload() any       { return r.payload }
func (r Registration) State() CatalogEntryState {
	return r.state
}

func nextCatalogID() string {
	return fmt.Sprintf("catalog-%d", catalogSequence.Add(1))
}

func nextLegacySource(name string) string {
	return fmt.Sprintf("legacy:%s:%d", name, legacySequence.Add(1))
}

// CatalogToolID binds allowlist authority to tool family and source.
func CatalogToolID(name, source string) string {
	kind := CatalogSourceKind(name, source)
	var value string
	switch kind {
	case "mcp":
		value = source + "/" + name
	default:
		value = kind + ":" + name
	}
	if len(value) <= 256 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return kind + ":" + hex.EncodeToString(sum[:])
}

func CatalogSourceKind(name, source string) string {
	switch {
	case strings.HasPrefix(source, "mcp:"):
		return "mcp"
	case strings.HasPrefix(source, "external:"):
		return "external"
	case strings.HasPrefix(source, "dynamic:"):
		return "dynamic"
	case name == "skills_read" || name == "skills_list" ||
		name == "skills.read" || name == "skills.list":
		return "skill"
	default:
		return "builtin"
	}
}

func ParseCatalogToolID(id string) (kind, name string, ok bool) {
	for _, candidate := range []string{
		"builtin", "mcp", "external", "skill", "dynamic",
	} {
		prefix := candidate + ":"
		if !strings.HasPrefix(id, prefix) || len(id) == len(prefix) {
			continue
		}
		name = strings.TrimPrefix(id, prefix)
		if candidate == "mcp" {
			if index := strings.LastIndexByte(name, '/'); index >= 0 {
				name = name[index+1:]
			}
		}
		return candidate, name, true
	}
	return "", "", false
}

// CatalogEntryState tracks exposure separately from availability.
type CatalogEntryState string

const (
	CatalogEntryEager        CatalogEntryState = "eager"
	CatalogEntryDeferred     CatalogEntryState = "deferred"
	CatalogEntryMaterialized CatalogEntryState = "materialized"
	CatalogEntryUnavailable  CatalogEntryState = "unavailable"
	CatalogEntryRevoked      CatalogEntryState = "revoked"
)

// CatalogEntrySnapshot binds a descriptor to source and authority revision.
type CatalogEntrySnapshot struct {
	Name          string             `json:"name"`
	Source        string             `json:"source"`
	Revision      uint64             `json:"revision"`
	State         CatalogEntryState  `json:"state"`
	External      ExternalDescriptor `json:"external_descriptor"`
	Descriptor    Descriptor         `json:"descriptor"`
	BindingDigest string             `json:"binding_digest"`
	authority     uint64
}

// PresentationDescriptor combines untrusted presentation fields with the
// Registry-frozen authority projection. Requested effects are never applied.
func (e CatalogEntrySnapshot) PresentationDescriptor() Descriptor {
	descriptor := cloneDescriptor(e.Descriptor)
	external := cloneExternalDescriptor(e.External)
	descriptor.Name = external.Name
	descriptor.Description = external.Description
	descriptor.DiscoveryTerms = external.DiscoveryTerms
	descriptor.InputSchema = external.InputSchema
	descriptor.Visibility = external.Visibility
	descriptor.Aliases = external.Aliases
	return descriptor
}

// CatalogSnapshot is an immutable, sorted sampling view.
type CatalogSnapshot struct {
	CatalogID  string
	Generation uint64
	Digest     string

	entries []CatalogEntrySnapshot
}

// NewCatalogSnapshot deep-clones descriptors for in-flight isolation.
func NewCatalogSnapshot(
	catalogID string,
	generation uint64,
	digest string,
	entries []CatalogEntrySnapshot,
) (CatalogSnapshot, error) {
	if catalogID == "" {
		return CatalogSnapshot{}, errors.New("catalog id is required")
	}
	if generation == 0 {
		return CatalogSnapshot{}, errors.New("catalog generation must be positive")
	}
	if digest == "" {
		return CatalogSnapshot{}, errors.New("catalog digest is required")
	}
	cloned := make([]CatalogEntrySnapshot, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if entry.Name == "" || entry.Source == "" || entry.Revision == 0 {
			return CatalogSnapshot{}, fmt.Errorf("catalog entry %d identity is incomplete", index)
		}
		if entry.Descriptor.Name != entry.Name {
			return CatalogSnapshot{}, fmt.Errorf(
				"catalog entry %q descriptor name is %q", entry.Name, entry.Descriptor.Name,
			)
		}
		if entry.External.Name == "" {
			entry.External = ExternalFromDescriptor(entry.Descriptor)
		}
		if entry.BindingDigest == "" {
			entry.BindingDigest = trustedBindingDigest(
				TrustedBindingFromDescriptor(entry.Descriptor),
			)
		}
		if entry.External.Name != entry.Name {
			return CatalogSnapshot{}, fmt.Errorf(
				"catalog entry %q external or binding identity is incomplete",
				entry.Name,
			)
		}
		if !validCatalogEntryState(entry.State) {
			return CatalogSnapshot{}, fmt.Errorf(
				"catalog entry %q has invalid state %q", entry.Name, entry.State,
			)
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return CatalogSnapshot{}, fmt.Errorf("catalog entry %q is duplicated", entry.Name)
		}
		if err := validateDescriptor(entry.Descriptor); err != nil {
			return CatalogSnapshot{}, fmt.Errorf("catalog entry %q: %w", entry.Name, err)
		}
		seen[entry.Name] = struct{}{}
		entry.Descriptor = cloneDescriptor(entry.Descriptor)
		entry.External = cloneExternalDescriptor(entry.External)
		cloned[index] = entry
	}
	sort.Slice(cloned, func(i, j int) bool { return cloned[i].Name < cloned[j].Name })
	return CatalogSnapshot{
		CatalogID: catalogID, Generation: generation, Digest: digest,
		entries: cloned,
	}, nil
}

func (s CatalogSnapshot) Entries() []CatalogEntrySnapshot {
	result := make([]CatalogEntrySnapshot, len(s.entries))
	for index, entry := range s.entries {
		entry.Descriptor = cloneDescriptor(entry.Descriptor)
		entry.External = cloneExternalDescriptor(entry.External)
		result[index] = entry
	}
	return result
}

func (s CatalogSnapshot) Lookup(name string) (CatalogEntrySnapshot, bool) {
	index := sort.Search(len(s.entries), func(index int) bool {
		return s.entries[index].Name >= name
	})
	if index == len(s.entries) || s.entries[index].Name != name {
		return CatalogEntrySnapshot{}, false
	}
	entry := s.entries[index]
	entry.Descriptor = cloneDescriptor(entry.Descriptor)
	entry.External = cloneExternalDescriptor(entry.External)
	return entry, true
}

type CatalogChange struct {
	Name     string
	Source   string
	Revision uint64
}

// ChangeSet is one name-sorted catalog generation transition.
type ChangeSet struct {
	CatalogID  string
	Generation uint64
	Digest     string
	Added      []CatalogChange
	Replaced   []CatalogChange
	Revoked    []CatalogChange
}

func DiffCatalog(before, after CatalogSnapshot) ChangeSet {
	changes := ChangeSet{
		CatalogID: after.CatalogID, Generation: after.Generation,
		Digest: after.Digest,
	}
	for _, entry := range after.Entries() {
		change := CatalogChange{
			Name: entry.Name, Source: entry.Source, Revision: entry.Revision,
		}
		previous, found := before.Lookup(entry.Name)
		switch {
		case !found:
			changes.Added = append(changes.Added, change)
		case previous.Revision != entry.Revision || previous.State != entry.State:
			changes.Replaced = append(changes.Replaced, change)
		}
	}
	for _, entry := range before.Entries() {
		if _, found := after.Lookup(entry.Name); !found {
			changes.Revoked = append(changes.Revoked, CatalogChange{
				Name: entry.Name, Source: entry.Source, Revision: entry.Revision,
			})
		}
	}
	return changes
}

func (r *Registry) CatalogID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.catalogID
}

func (r *Registry) Generation() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// Snapshot excludes revoked tombstones from the model-visible view.
func (r *Registry) Snapshot() (CatalogSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.tools {
		if err := r.refreshAvailabilityLocked(item); err != nil {
			return CatalogSnapshot{}, err
		}
	}
	entries := r.snapshotEntriesLocked()
	return NewCatalogSnapshot(
		r.catalogID, r.generation, catalogDigest(entries), entries,
	)
}

// SourceRegistrations preserves private tokens for CAS reconcile.
func (r *Registry) SourceRegistrations(source string) []Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sourceRegistrationsLocked(source)
}

// SourceState reads generation and registrations atomically.
func (r *Registry) SourceState(source string) (uint64, []Registration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation, r.sourceRegistrationsLocked(source)
}

// Reconcile atomically replaces every active entry owned by source.
func (r *Registry) Reconcile(
	source string,
	expectedGeneration uint64,
	registrations []Registration,
) (ChangeSet, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return ChangeSet{}, errors.New("tool catalog source is required")
	}
	normalized := make([]Registration, len(registrations))
	for index, registration := range registrations {
		if externalCatalogSource(source) && !registration.explicit {
			return ChangeSet{}, fmt.Errorf(
				"registration %d: external source %q requires a trusted binding",
				index, source,
			)
		}
		value, err := normalizeRegistration(registration)
		if err != nil {
			return ChangeSet{}, fmt.Errorf("registration %d: %w", index, err)
		}
		normalized[index] = value
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if expectedGeneration != 0 && expectedGeneration != r.generation {
		return ChangeSet{}, fmt.Errorf(
			"%w: expected=%d actual=%d",
			ErrCatalogStale, expectedGeneration, r.generation,
		)
	}
	return r.reconcileLocked(source, normalized)
}

func externalCatalogSource(source string) bool {
	return strings.HasPrefix(source, "mcp:") ||
		strings.HasPrefix(source, "external:") ||
		strings.HasPrefix(source, "dynamic:")
}

// registerOne is the O(1) path for immutable startup sources.
func (r *Registry) registerOne(source string, registration Registration) error {
	normalized, err := normalizeRegistration(registration)
	if err != nil {
		return err
	}
	name := normalized.descriptor.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists || r.aliases[name] != "" {
		return fmt.Errorf("tool %q is already registered", name)
	}
	for _, alias := range normalized.descriptor.Aliases {
		if _, exists := r.tools[alias.Name]; exists || r.aliases[alias.Name] != "" {
			return fmt.Errorf("tool alias %q is already registered", alias.Name)
		}
	}
	r.nextToken++
	item := &registered{
		descriptor: cloneDescriptor(normalized.descriptor),
		external:   cloneExternalDescriptor(normalized.external),
		binding:    cloneTrustedBinding(normalized.binding),
		executor:   normalized.executor,
		deferred:   normalized.deferred,
		backend:    r.backend,
		source:     source,
		revision:   1,
		state:      normalized.state,
		payload:    normalized.payload,
		token:      r.nextToken,
	}
	if item.deferred != nil {
		item.executor = descriptorExecutor{descriptor: cloneDescriptor(item.descriptor)}
	}
	item.wait = sync.NewCond(&r.mu)
	r.tools[name] = item
	for _, alias := range item.descriptor.Aliases {
		r.aliases[alias.Name] = name
	}
	delete(r.tombstones, name)
	for _, alias := range item.descriptor.Aliases {
		delete(r.tombstones, alias.Name)
	}
	r.generation++
	return nil
}

// Replace changes one entry without discarding the source's other entries.
func (r *Registry) Replace(
	source string,
	expectedGeneration uint64,
	registration Registration,
) (ChangeSet, error) {
	normalized, err := normalizeRegistration(registration)
	if err != nil {
		return ChangeSet{}, err
	}
	name := normalized.descriptor.Name
	r.mu.RLock()
	registrations := r.sourceRegistrationsLocked(source)
	generation := r.generation
	found := false
	for index := range registrations {
		if registrations[index].descriptor.Name == name {
			registrations[index] = normalized
			found = true
			break
		}
	}
	r.mu.RUnlock()
	if !found {
		return ChangeSet{}, fmt.Errorf("%w %q for source %q", ErrUnknownTool, name, source)
	}
	if expectedGeneration == 0 {
		expectedGeneration = generation
	}
	return r.Reconcile(source, expectedGeneration, registrations)
}

// Revoke removes one active entry and records canonical and alias tombstones.
func (r *Registry) Revoke(
	source string,
	name string,
	expectedGeneration uint64,
) (ChangeSet, error) {
	r.mu.RLock()
	if canonical := r.aliases[name]; canonical != "" {
		name = canonical
	}
	registrations := r.sourceRegistrationsLocked(source)
	generation := r.generation
	r.mu.RUnlock()
	filtered := make([]Registration, 0, len(registrations))
	found := false
	for _, registration := range registrations {
		if registration.descriptor.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, registration)
	}
	if !found {
		return ChangeSet{}, fmt.Errorf("%w %q for source %q", ErrUnknownTool, name, source)
	}
	if expectedGeneration == 0 {
		expectedGeneration = generation
	}
	return r.Reconcile(source, expectedGeneration, filtered)
}

func (r *Registry) reconcileLocked(
	source string,
	registrations []Registration,
) (ChangeSet, error) {
	desired := make(map[string]Registration, len(registrations))
	for _, registration := range registrations {
		name := registration.descriptor.Name
		if _, duplicate := desired[name]; duplicate {
			return ChangeSet{}, fmt.Errorf("tool %q is duplicated in source %q", name, source)
		}
		desired[name] = registration
		if current := r.tools[name]; current != nil && current.source != source {
			return ChangeSet{}, fmt.Errorf("tool %q is already registered", name)
		}
	}

	nextTools := make(map[string]*registered, len(r.tools)+len(desired))
	currentSource := make(map[string]*registered)
	for name, item := range r.tools {
		if item.source == source {
			currentSource[name] = item
			continue
		}
		nextTools[name] = item
	}
	nextTombstones := cloneTombstones(r.tombstones)
	changes := ChangeSet{CatalogID: r.catalogID}

	names := sortedRegistrationNames(desired)
	for _, name := range names {
		registration := desired[name]
		current := currentSource[name]
		if current != nil && sameRegistered(current, registration) {
			nextTools[name] = current
			delete(currentSource, name)
			continue
		}
		revision := uint64(1)
		change := &changes.Added
		if current != nil {
			revision = current.revision + 1
			change = &changes.Replaced
			delete(currentSource, name)
			for _, alias := range current.descriptor.Aliases {
				nextTombstones[alias.Name] = catalogTombstone{
					source: source, canonical: name, revision: revision,
				}
			}
		} else if tombstone, ok := nextTombstones[name]; ok && tombstone.source == source {
			revision = tombstone.revision + 1
		}
		r.nextToken++
		item := &registered{
			descriptor: cloneDescriptor(registration.descriptor),
			external:   cloneExternalDescriptor(registration.external),
			binding:    cloneTrustedBinding(registration.binding),
			executor:   registration.executor,
			deferred:   registration.deferred,
			backend:    r.backend,
			source:     source,
			revision:   revision,
			state:      registration.state,
			payload:    registration.payload,
			token:      r.nextToken,
		}
		if item.deferred != nil {
			item.executor = descriptorExecutor{descriptor: cloneDescriptor(item.descriptor)}
		}
		item.wait = sync.NewCond(&r.mu)
		nextTools[name] = item
		*change = append(*change, CatalogChange{Name: name, Source: source, Revision: revision})
	}

	for name, item := range currentSource {
		revision := item.revision + 1
		nextTombstones[name] = catalogTombstone{
			source: source, canonical: name, revision: revision,
		}
		for _, alias := range item.descriptor.Aliases {
			nextTombstones[alias.Name] = catalogTombstone{
				source: source, canonical: name, revision: revision,
			}
		}
		changes.Revoked = append(changes.Revoked, CatalogChange{
			Name: name, Source: source, Revision: revision,
		})
	}

	nextAliases, err := buildAliases(nextTools)
	if err != nil {
		return ChangeSet{}, err
	}
	for name, item := range nextTools {
		delete(nextTombstones, name)
		for _, alias := range item.descriptor.Aliases {
			delete(nextTombstones, alias.Name)
		}
	}
	if len(changes.Added) == 0 && len(changes.Replaced) == 0 && len(changes.Revoked) == 0 {
		changes.Generation = r.generation
		changes.Digest = catalogDigest(r.snapshotEntriesLocked())
		return changes, nil
	}
	sortCatalogChanges(changes.Added)
	sortCatalogChanges(changes.Replaced)
	sortCatalogChanges(changes.Revoked)
	r.tools = nextTools
	r.aliases = nextAliases
	r.tombstones = nextTombstones
	r.generation++
	changes.Generation = r.generation
	changes.Digest = catalogDigest(r.snapshotEntriesLocked())
	return changes, nil
}

func normalizeRegistration(registration Registration) (Registration, error) {
	if registration.explicit {
		if registration.external.Name == "" {
			switch {
			case registration.executor != nil:
				registration.external = ExternalFromDescriptor(
					registration.executor.Descriptor(),
				)
			case registration.descriptor.Name != "":
				registration.external = ExternalFromDescriptor(
					registration.descriptor,
				)
			}
		}
		if registration.descriptor.Name == "" {
			registration.descriptor = registration.external.Descriptor(
				registration.binding,
			)
		}
	}
	switch {
	case registration.executor != nil && registration.deferred != nil:
		return Registration{}, errors.New("tool registration cannot have both executor and loader")
	case registration.executor == nil && registration.deferred == nil:
		return Registration{}, errors.New("tool registration requires an executor or loader")
	case registration.deferred != nil:
		registration.descriptor.DeferredLoading.Enabled = true
		registration.descriptor.Availability = AvailabilityDeferred
		registration.external.Deferred.Enabled = true
		registration.external.Availability = AvailabilityDeferred
		registration.state = CatalogEntryDeferred
	default:
		live := registration.executor.Descriptor()
		if registration.explicit {
			if registration.external.Name == "" {
				registration.external = ExternalFromDescriptor(live)
			}
			if registration.state != CatalogEntryMaterialized {
				registration.descriptor = registration.external.Descriptor(
					registration.binding,
				)
			}
		} else {
			registration.descriptor = live
		}
		switch registration.descriptor.Availability {
		case AvailabilityUnavailable:
			registration.state = CatalogEntryUnavailable
		case AvailabilityDeferred:
			return Registration{}, errors.New("deferred descriptor requires a loader")
		default:
			if registration.state != CatalogEntryMaterialized {
				registration.state = CatalogEntryEager
			}
		}
	}
	if err := validateDescriptor(registration.descriptor); err != nil {
		return Registration{}, err
	}
	if !registration.explicit {
		registration.external = ExternalFromDescriptor(registration.descriptor)
		if provider, ok := registration.executor.(TrustedBindingProvider); ok {
			registration.binding = provider.TrustedBinding()
		} else {
			registration.binding = TrustedBindingFromDescriptor(
				registration.descriptor,
			)
		}
	} else if registration.external.Name == "" {
		registration.external = ExternalFromDescriptor(registration.descriptor)
	}
	if registration.external.Name != registration.descriptor.Name {
		return Registration{}, errors.New(
			"external descriptor and trusted binding name disagree",
		)
	}
	if err := registration.binding.Validate(); err != nil {
		return Registration{}, fmt.Errorf(
			"tool %q trusted binding: %w",
			registration.descriptor.Name, err,
		)
	}
	registration.descriptor = cloneDescriptor(registration.descriptor)
	registration.external = cloneExternalDescriptor(registration.external)
	registration.binding = cloneTrustedBinding(registration.binding)
	return registration, nil
}

func (r *Registry) sourceRegistrationsLocked(source string) []Registration {
	registrations := make([]Registration, 0)
	for _, item := range r.tools {
		if item.source != source {
			continue
		}
		registration := Registration{
			descriptor: cloneDescriptor(item.descriptor),
			external:   cloneExternalDescriptor(item.external),
			binding:    cloneTrustedBinding(item.binding),
			explicit:   true,
			state:      item.state,
			payload:    item.payload,
			token:      item.token,
		}
		if item.deferred != nil {
			registration.deferred = item.deferred
		} else {
			registration.executor = item.executor
		}
		registrations = append(registrations, registration)
	}
	sort.Slice(registrations, func(i, j int) bool {
		return registrations[i].descriptor.Name < registrations[j].descriptor.Name
	})
	return registrations
}

func (r *Registry) snapshotEntriesLocked() []CatalogEntrySnapshot {
	entries := make([]CatalogEntrySnapshot, 0, len(r.tools))
	for name, item := range r.tools {
		entries = append(entries, CatalogEntrySnapshot{
			Name: name, Source: item.source, Revision: item.revision,
			State: item.state, Descriptor: cloneDescriptor(item.descriptor),
			External:      cloneExternalDescriptor(item.external),
			BindingDigest: trustedBindingDigest(item.binding),
			authority:     item.token,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// Executors may tighten availability but cannot mutate authority or schema.
func (r *Registry) refreshAvailabilityLocked(item *registered) error {
	if item == nil || item.executor == nil || item.deferred != nil {
		return nil
	}
	live := item.executor.Descriptor()
	if live.Name != item.descriptor.Name {
		return fmt.Errorf(
			"registered tool %q now describes itself as %q",
			item.descriptor.Name, live.Name,
		)
	}
	if live.Availability == item.descriptor.Availability &&
		live.UnavailableReason == item.descriptor.UnavailableReason {
		return nil
	}
	candidate := cloneDescriptor(item.descriptor)
	candidate.Availability = live.Availability
	candidate.UnavailableReason = live.UnavailableReason
	if err := validateDescriptor(candidate); err != nil {
		return err
	}
	item.descriptor = candidate
	item.external.Availability = candidate.Availability
	item.external.Unavailable = candidate.UnavailableReason
	switch candidate.Availability {
	case AvailabilityUnavailable:
		item.state = CatalogEntryUnavailable
	default:
		if item.state == CatalogEntryMaterialized {
			item.state = CatalogEntryMaterialized
		} else {
			item.state = CatalogEntryEager
		}
	}
	item.revision++
	r.generation++
	return nil
}

func catalogDigest(entries []CatalogEntrySnapshot) string {
	data, _ := json.Marshal(entries)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sameRegistered(current *registered, desired Registration) bool {
	if current.state != desired.state ||
		!reflect.DeepEqual(current.descriptor, desired.descriptor) ||
		!reflect.DeepEqual(current.external, desired.external) ||
		!reflect.DeepEqual(current.binding, desired.binding) {
		return false
	}
	if current.deferred != nil || desired.deferred != nil {
		return current.deferred != nil && desired.deferred != nil &&
			desired.token != 0 && desired.token == current.token
	}
	return sameExecutor(current.executor, desired.executor)
}

func sameExecutor(left, right Executor) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	switch leftValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer,
		reflect.Slice, reflect.UnsafePointer:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		return leftValue.Type().Comparable() && leftValue.Interface() == rightValue.Interface()
	}
}

func buildAliases(tools map[string]*registered) (map[string]string, error) {
	aliases := make(map[string]string)
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, alias := range tools[name].descriptor.Aliases {
			if _, conflict := tools[alias.Name]; conflict {
				return nil, fmt.Errorf("tool alias %q conflicts with tool name", alias.Name)
			}
			if canonical := aliases[alias.Name]; canonical != "" {
				return nil, fmt.Errorf(
					"tool alias %q is already registered by %q", alias.Name, canonical,
				)
			}
			aliases[alias.Name] = name
		}
	}
	return aliases, nil
}

func sameAliases(left, right []Alias) bool {
	return reflect.DeepEqual(left, right)
}

func cloneTombstones(source map[string]catalogTombstone) map[string]catalogTombstone {
	result := make(map[string]catalogTombstone, len(source))
	maps.Copy(result, source)
	return result
}

func sortedRegistrationNames(registrations map[string]Registration) []string {
	names := make([]string, 0, len(registrations))
	for name := range registrations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortCatalogChanges(changes []CatalogChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
}

func validCatalogEntryState(state CatalogEntryState) bool {
	switch state {
	case CatalogEntryEager, CatalogEntryDeferred, CatalogEntryMaterialized,
		CatalogEntryUnavailable, CatalogEntryRevoked:
		return true
	default:
		return false
	}
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.InputSchema = cloneStringMap(descriptor.InputSchema)
	descriptor.Aliases = append([]Alias(nil), descriptor.Aliases...)
	descriptor.DiscoveryTerms = append([]string(nil), descriptor.DiscoveryTerms...)
	descriptor.IdentityKeys = append([]string(nil), descriptor.IdentityKeys...)
	descriptor.ResourceResolver.Templates = append(
		[]ResourceTemplate(nil), descriptor.ResourceResolver.Templates...,
	)
	return descriptor
}

func cloneStringMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneCatalogValue(value)
	}
	return result
}

func cloneCatalogValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneStringMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneCatalogValue(item)
		}
		return result
	case []string:
		result := make([]string, len(typed))
		copy(result, typed)
		return result
	default:
		return typed
	}
}
