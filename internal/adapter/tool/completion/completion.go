package completion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

const Name = "turn_complete"

type Tool struct{}

type output struct {
	Status      string                     `json:"status"`
	Message     string                     `json:"message"`
	Declaration tool.CompletionDeclaration `json:"-"`
}

func Register(registry *tool.Registry) error {
	executor, err := (&Tool{}).typedExecutor()
	if err != nil {
		return err
	}
	return registry.Register(executor)
}

func (*Tool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: Name,
		Description: "Choose the terminal state of the current Turn. With " +
			"status=complete, summary is the exact user-facing final response " +
			"(pending_actions must be empty) and the runtime publishes it " +
			"without another model sample. When captured narration is " +
			"available (for example after declaration repair or during " +
			"convergence finalization), output_mode=preserve_provisional " +
			"keeps the captured response and appends summary instead of " +
			"rewriting it. With status=incomplete, summary is a progress " +
			"summary and pending_actions records the concrete resumable " +
			"remainder as a blocked outcome.",
		Visibility:         tool.VisibleModel,
		Capability:         tool.CapabilityRead,
		AccessMode:         tool.AccessRead,
		ParallelPolicy:     tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{
					"type": "string", "enum": []string{"complete", "incomplete"},
				},
				"summary": map[string]any{
					"type": "string", "minLength": 1, "maxLength": 32768,
					"description": "Exact final response for complete; progress summary for incomplete.",
				},
				"output_mode": map[string]any{
					"type":        "string",
					"enum":        []string{"exact", "preserve_provisional"},
					"description": "Use preserve_provisional when captured narration from this Turn already states the answer; it is rejected when nothing was captured.",
				},
				"pending_actions": map[string]any{
					"type": "array", "maxItems": 32,
					"items": map[string]any{
						// One pending action is a prose bullet; the bound
						// matches the runtime's bounded-prose length and
						// stays far below the summary ceiling so an action
						// cannot become the answer itself.
						"type": "string", "minLength": 1, "maxLength": 512,
					},
				},
			},
			"required": []string{
				"status", "summary", "pending_actions",
			},
			"additionalProperties": false,
		},
	}
}

func (*Tool) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	executor, err := (&Tool{}).typedExecutor()
	if err != nil {
		return tool.Result{}, err
	}
	return executor.Execute(ctx, raw)
}

func (t *Tool) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[tool.CompletionDeclaration, output]{
		Descriptor:  t.Descriptor(),
		Disposition: tool.DispositionAbortImmediately,
		Validate:    validateDeclaration,
		Run: func(_ context.Context, declaration tool.CompletionDeclaration) (output, error) {
			return output{
				Status:      "pending_runtime_validation",
				Message:     "completion declaration submitted for runtime validation",
				Declaration: declaration,
			}, nil
		},
		Encode: func(value output) (tool.Result, error) {
			return toolresult.Success(value, nil)
		},
		Metadata: func(value output) map[string]any {
			return map[string]any{tool.MetadataCompletionDeclaration: value.Declaration}
		},
		Outcome: func(value output) tool.Outcome {
			declaration := value.Declaration
			return tool.Outcome{
				Status: tool.OutcomeSucceeded,
				Facts:  &tool.OutcomeFacts{Completion: &declaration},
			}
		},
	})
}

func validateDeclaration(declaration tool.CompletionDeclaration) error {
	switch declaration.OutputMode {
	case "", "exact", "preserve_provisional":
	default:
		return errors.New("unsupported completion output mode")
	}
	switch declaration.Status {
	case "complete":
		if len(declaration.PendingActions) != 0 {
			return errors.New(
				"complete declaration cannot contain pending actions",
			)
		}
	case "incomplete":
		if len(declaration.PendingActions) == 0 {
			return errors.New(
				"incomplete declaration requires pending actions",
			)
		}
		if declaration.OutputMode == "preserve_provisional" {
			return errors.New(
				"incomplete declaration cannot preserve provisional output",
			)
		}
	default:
		return errors.New("unsupported completion status")
	}
	for _, action := range declaration.PendingActions {
		if strings.TrimSpace(action) == "" {
			return errors.New("pending action cannot be empty")
		}
	}
	return nil
}
