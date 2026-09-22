package toolsearch

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type enabledKey struct{}

// WithEnabled scopes discovery to the same session/role filter used for model
// projection. It does not authorize execution or mutate the shared registry.
func WithEnabled(ctx context.Context, enabled func(tool.CatalogEntrySnapshot) bool) context.Context {
	return context.WithValue(ctx, enabledKey{}, enabled)
}

func modelAvailable(entry tool.CatalogEntrySnapshot, enabled func(tool.CatalogEntrySnapshot) bool) bool {
	return entry.State != tool.CatalogEntryRevoked &&
		entry.PresentationDescriptor().Visibility == tool.VisibleModel &&
		entry.Descriptor.Availability != tool.AvailabilityUnavailable &&
		(enabled == nil || enabled(entry))
}
