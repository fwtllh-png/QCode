package turnhistory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

const Name = "turn_history"

// Entry is the rendered history supplied by the runtime that owns the turn.
type Entry struct {
	Transcript    string
	FindingsIndex string
}

// Lookup returns nil when the turn is unavailable. Visibility and rendering
// are owned by the caller; the tool only validates and pages the result.
type Lookup func(ctx context.Context, turn uint64) (*Entry, error)

type ReferenceRequest struct {
	SourceID  string `json:"source_id,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	Catalog   bool   `json:"catalog,omitempty"`
	IndexOnly bool   `json:"index_only,omitempty"`
}

type ReferenceLookup func(context.Context, ReferenceRequest) (*Entry, error)

type input struct {
	ReferenceRequest
	Turn     uint64 `json:"turn,omitempty"`
	From     string `json:"from,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	Offset   *int   `json:"offset,omitempty"`
	Digest   string `json:"content_digest,omitempty"`
}

func Register(registry *tool.Registry, lookup Lookup, references ...ReferenceLookup) error {
	if registry == nil {
		return errors.New("turn_history requires a registry")
	}
	if lookup == nil {
		return errors.New("turn_history requires a history lookup")
	}
	if _, _, _, err := registry.Resolve(Name); err == nil {
		return nil
	}
	executor, err := typed.Define(typed.Spec[input, tool.Result]{
		Descriptor: tool.Descriptor{
			Name: Name,
			Description: "Read conversation sources using exactly one selector: turn, source_id, item_id, or catalog=true. " +
				"Turn reads default to the transcript tail and findings; source/item/catalog reads default to head. " +
				"index_only=true returns a source's item index. Incomplete transcripts are labeled. " +
				"max_bytes bounds returned content; 0 uses normal result admission. Continue with the same selector, " +
				"offset=next_offset and content_digest from page metadata. To read before an initial tail, use from=head. " +
				"result_get retrieves only the retained tool page, not omitted source bytes. Recover definitions before acting; " +
				"do not search the workspace for conversation-only lists.",
			Visibility:         tool.VisibleModel,
			Capability:         tool.CapabilityRead,
			AccessMode:         tool.AccessRead,
			ParallelPolicy:     tool.ParallelConcurrent,
			RepeatPolicy:       tool.RepeatReplaySameTurn,
			SandboxRequirement: tool.SandboxNone,
			Availability:       tool.AvailabilityAvailable,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"turn":           map[string]any{"type": "integer", "minimum": float64(1)},
					"source_id":      map[string]any{"type": "string", "minLength": float64(1)},
					"item_id":        map[string]any{"type": "string", "minLength": float64(1)},
					"catalog":        map[string]any{"type": "boolean", "enum": []any{true}},
					"index_only":     map[string]any{"type": "boolean"},
					"offset":         map[string]any{"type": "integer", "minimum": float64(0)},
					"content_digest": map[string]any{"type": "string"},
					"from": map[string]any{
						"type": "string", "enum": []any{"tail", "head"},
					},
					"max_bytes": map[string]any{"type": "integer", "minimum": float64(0)},
				},
				"oneOf":                []any{map[string]any{"required": []string{"turn"}}, map[string]any{"required": []string{"source_id"}}, map[string]any{"required": []string{"item_id"}}, map[string]any{"required": []string{"catalog"}}},
				"additionalProperties": false,
			},
		},
		Disposition: tool.DispositionWaitForTeardown,
		Validate: func(value input) error {
			selectors := 0
			for _, present := range []bool{value.Turn != 0, value.SourceID != "", value.ItemID != "", value.Catalog} {
				if present {
					selectors++
				}
			}
			if selectors != 1 {
				return errors.New("exactly one of turn, source_id, item_id or catalog is required")
			}
			if value.IndexOnly && value.SourceID == "" {
				return errors.New("index_only requires source_id")
			}
			if value.Offset != nil && (*value.Offset < 0 || value.Digest == "" || value.From != "") {
				return errors.New("offset requires content_digest, nonnegative bytes, and no from")
			}
			if value.From != "" && value.From != "tail" && value.From != "head" {
				return errors.New("from must be tail or head")
			}
			if value.MaxBytes < 0 {
				return errors.New("max_bytes must not be negative")
			}
			return nil
		},
		Run: func(ctx context.Context, value input) (tool.Result, error) {
			current := runtimeLookup{turn: lookup}
			if len(references) != 0 {
				current.references = references[0]
			}
			if scoped, ok := ctx.Value(lookupKey{}).(runtimeLookup); ok {
				current = scoped
			}
			var entry *Entry
			var err error
			if value.Turn != 0 {
				if current.turn == nil {
					return tool.Result{}, errors.New("turn history lookup is unavailable")
				}
				entry, err = current.turn(ctx, value.Turn)
			} else if current.references != nil {
				entry, err = current.references(ctx, value.ReferenceRequest)
			} else {
				return tool.Result{}, errors.New("conversation source lookup is unavailable")
			}
			if err != nil {
				return tool.Result{}, err
			}
			if entry == nil {
				return tool.Result{}, fmt.Errorf("requested source or turn %d is not in durable history", value.Turn)
			}
			return referencePage(entry, value)
		},
		Encode: func(value tool.Result) (tool.Result, error) {
			return value, nil
		},
	})
	if err != nil {
		return err
	}
	return registry.Register(executor)
}

func assemblePage(transcript, index, from string, maxBytes int) (string, bool, int) {
	index = strings.TrimSpace(index)
	if index == "" {
		if maxBytes > 0 && len(transcript) > maxBytes {
			if from == "head" {
				return truncateUTF8(transcript, maxBytes), true, len(transcript)
			}
			return truncateUTF8Tail(transcript, maxBytes), true, len(transcript)
		}
		return transcript, false, len(transcript)
	}
	combined := strings.TrimRight(transcript, "\n") + "\n\n" + index + "\n"
	if maxBytes <= 0 || len(combined) <= maxBytes {
		return combined, false, len(combined)
	}
	if len(index)+3 >= maxBytes {
		return truncateUTF8Tail(combined, maxBytes), true, len(combined)
	}
	reserved := len(index) + 3
	bodyBudget := max(0, maxBytes-reserved)
	var body string
	if from == "head" {
		body = truncateUTF8(transcript, bodyBudget)
	} else {
		body = truncateUTF8Tail(transcript, bodyBudget)
	}
	return strings.TrimRight(body, "\n") + "\n\n" + index + "\n", true, len(combined)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func truncateUTF8Tail(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
}

func referencePage(entry *Entry, value input) (tool.Result, error) {
	body, _, _ := assemblePage(entry.Transcript, entry.FindingsIndex, "head", 0)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
	if value.Digest != "" && value.Digest != digest {
		return tool.Result{}, errors.New("history content changed; restart paging from the source")
	}
	start := 0
	if value.Offset != nil {
		start = *value.Offset
		if start > len(body) || !utf8.ValidString(body[:start]) {
			return tool.Result{}, errors.New("offset is outside the source or inside a UTF-8 character")
		}
	} else if value.From == "tail" || value.From == "" && value.Turn != 0 {
		if value.MaxBytes > 0 {
			start = max(0, len(body)-value.MaxBytes)
			for start < len(body) && !utf8.RuneStart(body[start]) {
				start++
			}
		}
	}
	content := body[start:]
	if value.MaxBytes > 0 {
		content = truncateUTF8(content, value.MaxBytes)
	}
	if content == "" && len(body) != 0 && (value.Offset == nil || start < len(body)) {
		return tool.Result{}, errors.New("max_bytes cannot fit the next UTF-8 character")
	}
	end := start + len(content)
	metadata := map[string]any{"content_digest": digest, "offset": start, "end_offset": end, "has_more": start > 0 || end < len(body), "original_bytes": len(body)}
	if end < len(body) {
		metadata["next_offset"] = end
	}
	if start > 0 {
		metadata["previous_offset"] = 0
	}
	return tool.Result{Content: content, Truncated: start > 0 || end < len(body), OriginalBytes: len(body), Metadata: metadata}, nil
}
