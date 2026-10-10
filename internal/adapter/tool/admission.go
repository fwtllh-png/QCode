package tool

import "fmt"

// WithCurrentBinding fences catalog replacement through physical process start.
// start must not call back into Registry.
func (r *Registry) WithCurrentBinding(ref ToolRef, requireGeneration bool, start func() error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item := r.tools[ref.Name]
	if item == nil || item.source != ref.Source || item.revision != ref.Revision ||
		item.token != ref.Authority || r.catalogID != ref.CatalogID || (requireGeneration && r.generation != ref.Generation) {
		return fmt.Errorf("%w: binding changed before process start", ErrCatalogStale)
	}
	return start()
}
