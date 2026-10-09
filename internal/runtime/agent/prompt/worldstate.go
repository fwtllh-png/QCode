package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

const (
	PartitionPolicy      = "policy"
	PartitionToolCatalog = "tool_catalog"
)

func digestJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%v", value)))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PolicySection snapshots mode/permission/granular for WorldState.
type PolicySection struct {
	Mode       string `json:"mode"`
	Permission string `json:"permission"`
	policy.PlanningSnapshot
	Granular policy.Granular `json:"granular"`
}

func NewPolicySection(runtime *policy.Runtime) PolicySection {
	if runtime == nil {
		return PolicySection{}
	}
	return PolicySection{
		Mode: string(runtime.Mode), Permission: string(runtime.Permission),
		PlanningSnapshot: runtime.PlanningSnapshot(), Granular: runtime.Granular,
	}
}

func (p PolicySection) ID() string { return PartitionPolicy }

func (p PolicySection) Digest() string { return digestJSON(p) }

func (p PolicySection) Render() string {
	var b strings.Builder
	b.WriteString("Policy snapshot:\n")
	permission := p.Permission
	if p.Permission == "bypass" {
		permission += " (Full Access)"
	}
	b.WriteString(fmt.Sprintf("- mode=%s permission=%s\n", p.Mode, permission))
	b.WriteString(p.Guidance())
	b.WriteString(fmt.Sprintf(
		"- granular: sandbox=%s rules=%s skills=%s mcp=%s\n",
		p.Granular.Sandbox.Label(), p.Granular.Rules.Label(),
		p.Granular.Skills.Label(), p.Granular.MCP.Label(),
	))
	return strings.TrimSpace(b.String())
}

// ToolCatalogEntry is one model-visible tool line for WorldState.
type ToolCatalogEntry struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Availability string `json:"availability"`
	Capability   string `json:"capability,omitempty"`
}

// ToolCatalogSection lists visible tools (including deferred) for cache-aware diffs.
type ToolCatalogSection struct {
	Entries           []ToolCatalogEntry `json:"entries"`
	Materialized      []string           `json:"materialized,omitempty"`
	DeferredAvailable int                `json:"deferred_available,omitempty"`
	Omitted           int                `json:"omitted,omitempty"`
}

func NewToolCatalogSection(registry *tool.Registry) ToolCatalogSection {
	if registry == nil {
		return ToolCatalogSection{}
	}
	descriptors := registry.Descriptors(tool.VisibleModel)
	entries := make([]ToolCatalogEntry, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Availability == tool.AvailabilityUnavailable {
			continue
		}
		desc := strings.TrimSpace(descriptor.Description)
		if len(desc) > 120 {
			desc = desc[:117] + "..."
		}
		entries = append(entries, ToolCatalogEntry{
			Name: descriptor.Name, Description: desc,
			Availability: string(descriptor.Availability),
			Capability:   string(descriptor.Capability),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return ToolCatalogSection{Entries: entries}
}

func NewToolCatalogSectionFromSnapshot(
	snapshot tool.CatalogSnapshot,
	advertised map[string]bool,
) ToolCatalogSection {
	var section ToolCatalogSection
	for _, entry := range snapshot.Entries() {
		presentation := entry.PresentationDescriptor()
		if presentation.Visibility != tool.VisibleModel ||
			entry.Descriptor.Availability == tool.AvailabilityUnavailable {
			continue
		}
		if entry.State == tool.CatalogEntryDeferred {
			section.DeferredAvailable++
		}
		if entry.State == tool.CatalogEntryMaterialized {
			section.Materialized = append(section.Materialized, entry.Name)
		}
		if !advertised[entry.Name] {
			section.Omitted++
			continue
		}
		desc := strings.TrimSpace(presentation.Description)
		if len(desc) > 120 {
			desc = desc[:117] + "..."
		}
		section.Entries = append(section.Entries, ToolCatalogEntry{
			Name: entry.Name, Description: desc,
			Availability: string(entry.Descriptor.Availability),
			Capability:   string(entry.Descriptor.Capability),
		})
	}
	sort.Strings(section.Materialized)
	sort.Slice(section.Entries, func(i, j int) bool {
		return section.Entries[i].Name < section.Entries[j].Name
	})
	return section
}

// AssembleToolCatalog renders the volatile catalog tail after history.
func AssembleToolCatalog(
	section ToolCatalogSection,
	budget Budget,
) ([]provider.Message, Receipt) {
	messages, receipt := AssembleWorldText(
		PartitionToolCatalog,
		"session://tool-catalog",
		section.Render(),
		budget,
	)
	receipt.Digest = section.Digest()
	return messages, receipt
}

// AssembleWorldText applies one section budget without deciding whether the
// section changed. ContextStore's WorldBaseline is the only diff authority.
func AssembleWorldText(
	kind string,
	source string,
	text string,
	budget Budget,
) ([]provider.Message, Receipt) {
	tokens := HeuristicTokenCounter{}
	retained, reason := retain(text, budget, len(truncationNotice), 0, tokens)
	receipt := newReceipt(
		kind, source, text, retained, reason, tokens,
	)
	if reason != "" {
		retained += truncationNotice
	}
	if strings.TrimSpace(retained) == "" {
		return nil, receipt
	}
	return []provider.Message{provider.TextMessage(provider.RoleSystem, retained)}, receipt
}

func (t ToolCatalogSection) Digest() string {
	return digestJSON(struct {
		Advertised        int `json:"advertised"`
		Materialized      int `json:"materialized"`
		DeferredAvailable int `json:"deferred_available"`
		Omitted           int `json:"omitted"`
	}{
		Advertised: len(t.Entries), Materialized: len(t.Materialized),
		DeferredAvailable: t.DeferredAvailable, Omitted: t.Omitted,
	})
}

func (t ToolCatalogSection) Render() string {
	if len(t.Entries) == 0 && len(t.Materialized) == 0 &&
		t.DeferredAvailable == 0 && t.Omitted == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[tool_catalog]\n")
	fmt.Fprintf(
		&b,
		"advertised=%d materialized=%d deferred_available=%d omitted=%d\n",
		len(t.Entries),
		len(t.Materialized),
		t.DeferredAvailable,
		t.Omitted,
	)
	if t.DeferredAvailable > 0 || t.Omitted > 0 {
		b.WriteString("Use tool_search to load omitted tools.\n")
	}
	return strings.TrimSpace(b.String())
}
