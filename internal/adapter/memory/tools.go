package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

type Tool struct {
	store *Store
}

type input struct {
	Note      string `json:"note"`
	Scope     string `json:"scope,omitempty"`
	Category  string `json:"category,omitempty"`
	Source    string `json:"source,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type output struct {
	Content string
	Path    string
}

func New(store *Store) (*Tool, error) {
	if store == nil {
		return nil, errors.New("remember store is required")
	}
	return &Tool{store: store}, nil
}

func Register(registry *tool.Registry, store *Store) error {
	if registry == nil {
		return errors.New("remember registry is required")
	}
	builders := []func(*Store) (tool.Executor, error){
		func(store *Store) (tool.Executor, error) {
			value, err := New(store)
			if err != nil {
				return nil, err
			}
			return value.typedExecutor()
		},
		newListExecutor,
		newGetExecutor,
		newUpdateExecutor,
		newForgetExecutor,
	}
	for _, build := range builders {
		executor, err := build(store)
		if err != nil {
			return err
		}
		if err := registry.RegisterTrusted(
			"builtin:memory", trustedRegistration(executor),
		); err != nil {
			return err
		}
	}
	return nil
}

func trustedRegistration(executor tool.Executor) tool.Registration {
	descriptor := executor.Descriptor()
	binding := tool.TrustedBindingFromDescriptor(descriptor)
	if provider, ok := executor.(tool.TrustedBindingProvider); ok {
		binding = provider.TrustedBinding()
	}
	return tool.NewExternalRegistration(
		tool.ExternalFromDescriptor(descriptor),
		binding,
		executor,
	)
}

func (t *Tool) Descriptor() tool.Descriptor {
	resourceID := t.store.Path()
	if os.Getenv("QCODE_HERMETIC_MANIFEST") == "1" {
		resourceID = filepath.ToSlash(filepath.Join(".qcode", RecordsFileName))
	}
	return tool.Descriptor{
		Name: "remember",
		Description: "Append a durable note to the user memory file so it surfaces in " +
			"future sessions. Use this when the user states a preference, convention, or " +
			"fact that should persist. Keep notes terse. Do not store secrets.",
		DiscoveryTerms: []string{"remember", "memory", "记住", "记忆", "偏好"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"note": map[string]any{
					"type":      "string",
					"minLength": float64(1),
					"maxLength": float64(MaxNoteBytes),
				},
				"scope": map[string]any{
					"type": "string",
					"enum": []string{"user", "workspace", "repository"},
				},
				"category": map[string]any{
					"type": "string",
					"enum": []string{"preference", "convention", "fact"},
				},
				"source":     map[string]any{"type": "string", "maxLength": float64(256)},
				"expires_at": map[string]any{"type": "string"},
			},
			"required":             []string{"note"},
			"additionalProperties": false,
		},
		Visibility: tool.VisibleModel,
		Capability: tool.CapabilityWrite,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "memory", ID: resourceID, Access: tool.AccessWrite,
		}}},
		AccessMode:         tool.AccessWrite,
		ParallelPolicy:     tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
	}
}

func (t *Tool) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[input, output]{
		Descriptor:  t.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value input) (output, error) {
			expiresAt, err := parseExpiry(value.ExpiresAt)
			if err != nil {
				return output{}, err
			}
			record, _, err := t.store.Remember(CreateRequest{
				Scope:    Scope(value.Scope),
				Category: Category(value.Category),
				Text:     value.Note, Source: value.Source, ExpiresAt: expiresAt,
			})
			if err != nil {
				return output{}, err
			}
			return output{
				Content: "remembered " + record.ID + ": " + record.Text,
				Path:    t.store.Path(),
			}, nil
		},
		Encode: func(value output) (tool.Result, error) {
			return toolresult.Text(value.Content, nil), nil
		},
		Metadata: func(value output) map[string]any {
			return map[string]any{"path": value.Path}
		},
	})
}

type queryInput struct {
	Query         string   `json:"query,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Category      string   `json:"category,omitempty"`
	PinnedIDs     []string `json:"pinned_ids,omitempty"`
	MaxCandidates int      `json:"max_candidates,omitempty"`
}

type idInput struct {
	ID string `json:"id"`
}

type updateInput struct {
	ID        string  `json:"id"`
	Text      string  `json:"text"`
	Category  string  `json:"category,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

func newListExecutor(store *Store) (tool.Executor, error) {
	descriptor := memoryDescriptor(
		store,
		"memory_list",
		"List durable memory metadata selected for the current scope without returning record bodies.",
		tool.AccessRead,
		map[string]any{
			"query": map[string]any{"type": "string"},
			"scope": map[string]any{
				"type": "string",
				"enum": []string{"user", "workspace", "repository"},
			},
			"category": map[string]any{
				"type": "string",
				"enum": []string{"preference", "convention", "fact"},
			},
			"pinned_ids": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": float64(1), "maximum": float64(1000),
			},
		},
		nil,
	)
	return typed.Define(typed.Spec[queryInput, map[string]any]{
		Descriptor: descriptor, Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value queryInput) (map[string]any, error) {
			records, generation, err := store.List(Query{
				Text: value.Query, PinnedIDs: value.PinnedIDs,
				Scope:         Scope(value.Scope),
				Category:      Category(value.Category),
				MaxCandidates: value.MaxCandidates,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"generation": generation,
				"records":    records,
			}, nil
		},
		Encode: func(value map[string]any) (tool.Result, error) {
			return toolresult.Success(value, nil)
		},
	})
}

func newGetExecutor(store *Store) (tool.Executor, error) {
	return typed.Define(typed.Spec[idInput, MemoryRecord]{
		Descriptor: memoryDescriptor(
			store,
			"memory_get",
			"Read one durable memory record by its exact id.",
			tool.AccessRead,
			map[string]any{"id": map[string]any{"type": "string", "minLength": float64(1)}},
			[]string{"id"},
		),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value idInput) (MemoryRecord, error) {
			return store.Get(value.ID)
		},
	})
}

func newUpdateExecutor(store *Store) (tool.Executor, error) {
	return typed.Define(typed.Spec[updateInput, MemoryRecord]{
		Descriptor: memoryDescriptor(
			store,
			"memory_update",
			"Update one durable memory record by id without changing its scope.",
			tool.AccessWrite,
			map[string]any{
				"id": map[string]any{"type": "string", "minLength": float64(1)},
				"text": map[string]any{
					"type": "string", "minLength": float64(1),
					"maxLength": float64(MaxNoteBytes),
				},
				"category": map[string]any{
					"type": "string",
					"enum": []string{"preference", "convention", "fact"},
				},
				"expires_at": map[string]any{"type": "string"},
			},
			[]string{"id", "text"},
		),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value updateInput) (MemoryRecord, error) {
			var expiresAt *time.Time
			if value.ExpiresAt != nil {
				var err error
				expiresAt, err = parseExpiry(*value.ExpiresAt)
				if err != nil {
					return MemoryRecord{}, err
				}
			}
			return store.Update(UpdateRequest{
				ID: value.ID, Text: value.Text,
				Category:  Category(value.Category),
				ExpiresAt: expiresAt,
				SetExpiry: value.ExpiresAt != nil,
			})
		},
	})
}

func newForgetExecutor(store *Store) (tool.Executor, error) {
	return typed.Define(typed.Spec[idInput, map[string]any]{
		Descriptor: memoryDescriptor(
			store,
			"forget",
			"Delete one durable memory record by its exact id.",
			tool.AccessWrite,
			map[string]any{"id": map[string]any{"type": "string", "minLength": float64(1)}},
			[]string{"id"},
		),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value idInput) (map[string]any, error) {
			deleted, generation, err := store.Forget(value.ID)
			return map[string]any{
				"id": value.ID, "deleted": deleted, "generation": generation,
			}, err
		},
		Encode: func(value map[string]any) (tool.Result, error) {
			return toolresult.Success(value, nil)
		},
	})
}

func memoryDescriptor(
	store *Store,
	name string,
	description string,
	access tool.AccessMode,
	properties map[string]any,
	required []string,
) tool.Descriptor {
	capability := tool.CapabilityRead
	if access == tool.AccessWrite {
		capability = tool.CapabilityWrite
	}
	resourceID := store.Path()
	if os.Getenv("QCODE_HERMETIC_MANIFEST") == "1" {
		resourceID = filepath.ToSlash(filepath.Join(".qcode", RecordsFileName))
	}
	return tool.Descriptor{
		Name: name, Description: description,
		DiscoveryTerms: []string{"memory", "remember", "记忆", "记住"},
		InputSchema: map[string]any{
			"type": "object", "properties": properties,
			"required": required, "additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: capability,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "memory", ID: resourceID, Access: access,
		}}},
		AccessMode: access, ParallelPolicy: tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
	}
}

func parseExpiry(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, errors.New("expires_at must use RFC3339")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}
