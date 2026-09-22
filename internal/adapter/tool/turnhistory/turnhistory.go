package turnhistory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

const Name = agentcontext.TurnHistoryToolName

type Lookup func(ctx context.Context, turn uint64) ([]provider.Message, error)

type FindingsLookup func(ctx context.Context, turn uint64) (agentcontext.TurnFindings, bool)

type input struct {
	Turn     uint64 `json:"turn"`
	From     string `json:"from,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func Register(registry *tool.Registry, lookup Lookup, findings ...FindingsLookup) error {
	if registry == nil {
		return errors.New("turn_history requires a registry")
	}
	if lookup == nil {
		return errors.New("turn_history requires a history lookup")
	}
	if _, _, _, err := registry.Resolve(Name); err == nil {
		return nil
	}
	var findingsLookup FindingsLookup
	if len(findings) > 0 {
		findingsLookup = findings[0]
	}
	executor, err := typed.Define(typed.Spec[input, tool.Result]{
		Descriptor: tool.Descriptor{
			Name: Name,
			Description: "Read a closed turn's durable transcript by turn id. " +
				"The first page is the turn tail plus a findings index " +
				"(conclusion and sites) at the end. Use from=head only for " +
				"the start of the turn. If truncated, page with result_get " +
				"mode=tail or mode=query (for example query=sites or " +
				"query=P2); default result_get mode=summary does not " +
				"reconstruct lists. Do not search the workspace for " +
				"conversation-only lists. First write is final.",
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
					"turn": map[string]any{"type": "integer", "minimum": float64(1)},
					"from": map[string]any{
						"type": "string", "enum": []any{"tail", "head"},
					},
					"max_bytes": map[string]any{"type": "integer", "minimum": float64(0)},
				},
				"required":             []string{"turn"},
				"additionalProperties": false,
			},
		},
		Disposition: tool.DispositionWaitForTeardown,
		Validate: func(value input) error {
			if value.Turn == 0 {
				return errors.New("turn is required")
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
			messages, err := lookup(ctx, value.Turn)
			if err != nil {
				return tool.Result{}, err
			}
			if len(messages) == 0 {
				return tool.Result{}, fmt.Errorf("turn %d is not in durable history", value.Turn)
			}
			var findings agentcontext.TurnFindings
			if findingsLookup != nil {
				findings, _ = findingsLookup(ctx, value.Turn)
			}
			content, truncated, original := assemblePage(
				agentcontext.RenderTurnTranscript(messages),
				agentcontext.RenderTurnFindings(value.Turn, findings),
				value.From,
				value.MaxBytes,
			)
			if truncated {
				return tool.Result{
					Content: content, Truncated: true, OriginalBytes: original,
				}, nil
			}
			return tool.Result{Content: content}, nil
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
				return agentcontext.TruncateUTF8(transcript, maxBytes), true, len(transcript)
			}
			return agentcontext.TruncateUTF8Tail(transcript, maxBytes), true, len(transcript)
		}
		return transcript, false, len(transcript)
	}
	combined := strings.TrimRight(transcript, "\n") + "\n\n" + index + "\n"
	if maxBytes <= 0 || len(combined) <= maxBytes {
		return combined, false, len(combined)
	}
	reserved := len(index) + 2
	bodyBudget := max(0, maxBytes-reserved)
	var body string
	if from == "head" {
		body = agentcontext.TruncateUTF8(transcript, bodyBudget)
	} else {
		body = agentcontext.TruncateUTF8Tail(transcript, bodyBudget)
	}
	return strings.TrimRight(body, "\n") + "\n\n" + index + "\n", true, len(combined)
}
