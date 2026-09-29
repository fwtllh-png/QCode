package typed

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

type fixtureInput struct {
	Name string `json:"name"`
}

type fixtureOutput struct {
	Greeting string `json:"greeting"`
}

type embeddedFixture struct {
	Contract[fixtureInput, tool.Result]
	descriptor tool.Descriptor
}

func (f embeddedFixture) Descriptor() tool.Descriptor {
	return f.descriptor
}

func TestDefineExecutesStrictTypedContract(t *testing.T) {
	executor := fixtureExecutor(t, func(_ context.Context, input fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{Greeting: "hello " + input.Name}, nil
	})
	value, err := executor.Execute(t.Context(), json.RawMessage(`{"name":"Ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	if value.Content != `{"greeting":"hello Ada"}` || value.Metadata["greeting"] != "hello Ada" {
		t.Fatalf("result = %+v", value)
	}
	if value.Outcome == nil || value.Outcome.Status != tool.OutcomeSucceeded {
		t.Fatalf("typed outcome = %+v", value.Outcome)
	}
	if disposition := tool.DispositionFor(executor); disposition != tool.DispositionAbortImmediately {
		t.Fatalf("execution disposition = %q", disposition)
	}
	for _, raw := range []string{
		`{"name":"Ada","unknown":true}`,
		`{"name":"Ada"} {"name":"Grace"}`,
		`null`,
	} {
		if _, err := executor.Execute(t.Context(), json.RawMessage(raw)); !errors.Is(err, tool.ErrInvalidArguments) {
			t.Fatalf("arguments %q error = %v", raw, err)
		}
	}
}

func TestResultContractEmbedsInSpecializedTool(t *testing.T) {
	descriptor := fixtureSpec(nil).Descriptor
	contract, err := NewResultContract(ResultSpec[fixtureInput]{
		Name: descriptor.Name, Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, input fixtureInput) (tool.Result, error) {
			return tool.Result{Content: input.Name}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := embeddedFixture{Contract: contract, descriptor: descriptor}
	result, outcome, err := executor.ExecuteOutcome(
		t.Context(),
		json.RawMessage(`{"name":"embedded"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "embedded" || outcome.Status != tool.OutcomeSucceeded {
		t.Fatalf("result = %+v, outcome = %+v", result, outcome)
	}
	if disposition := tool.DispositionFor(executor); disposition != tool.DispositionWaitForTeardown {
		t.Fatalf("execution disposition = %q", disposition)
	}
	if _, err := executor.Execute(
		t.Context(),
		json.RawMessage(`{"name":"embedded","extra":true}`),
	); !errors.Is(err, tool.ErrInvalidArguments) {
		t.Fatalf("strict decode error = %v", err)
	}
}

func TestRegistryKeepsSchemaAndOutputRouting(t *testing.T) {
	registry := tool.NewRegistry(nil, tool.NewResultStore(16))
	executor := fixtureExecutor(t, func(_ context.Context, input fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{Greeting: strings.Repeat(input.Name, 32)}, nil
	})
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"name":"x","unknown":true}`} {
		if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
			Name: "typed_fixture", Arguments: json.RawMessage(raw),
		}); !errors.Is(err, tool.ErrInvalidArguments) {
			t.Fatalf("schema arguments %s error = %v", raw, err)
		}
	}
	value, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "typed_fixture", Arguments: json.RawMessage(`{"name":"long"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !value.Truncated || value.Handle == "" || value.OriginalBytes <= 16 {
		t.Fatalf("large result was not routed: %+v", value)
	}
}

func TestExecutorContainsPanicCancellationAndInvalidMetadata(t *testing.T) {
	panicExecutor := fixtureExecutor(t, func(context.Context, fixtureInput) (fixtureOutput, error) {
		panic("boom")
	})
	if _, err := panicExecutor.Execute(t.Context(), json.RawMessage(`{"name":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := panicExecutor.Execute(ctx, json.RawMessage(`{"name":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	spec := fixtureSpec(func(_ context.Context, _ fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{}, nil
	})
	spec.Metadata = func(fixtureOutput) map[string]any {
		return map[string]any{"bad": make(chan struct{})}
	}
	invalid, err := Define(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Execute(t.Context(), json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Fatal("non-JSON metadata succeeded")
	}
}

func TestDefineValidatesDescriptorAndRequiredRun(t *testing.T) {
	spec := fixtureSpec(func(context.Context, fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{}, nil
	})
	spec.Descriptor.Availability = ""
	if _, err := Define(spec); err == nil {
		t.Fatal("invalid descriptor succeeded")
	}
	spec = fixtureSpec(nil)
	if _, err := Define(spec); err == nil {
		t.Fatal("missing Run succeeded")
	}
	spec = fixtureSpec(func(context.Context, fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{}, nil
	})
	spec.Descriptor.RepeatPolicy = ""
	if _, err := Define(spec); err == nil {
		t.Fatal("implicit RepeatPolicy succeeded")
	}
	spec = fixtureSpec(func(context.Context, fixtureInput) (fixtureOutput, error) {
		return fixtureOutput{}, nil
	})
	spec.Disposition = ""
	if _, err := Define(spec); err == nil {
		t.Fatal("implicit execution disposition succeeded")
	}
	if _, err := NewResultContract(ResultSpec[fixtureInput]{
		Disposition: tool.DispositionAbortImmediately,
		Run: func(context.Context, fixtureInput) (tool.Result, error) {
			return tool.Result{}, nil
		},
	}); err == nil {
		t.Fatal("missing contract name succeeded")
	}
}

func TestDefaultEncoderRejectsNonFiniteJSONNumbers(t *testing.T) {
	result, err := EncodeJSON(map[string]float64{"invalid": math.NaN()})
	if err == nil || result.Content != "" || result.Metadata != nil {
		t.Fatalf("non-finite JSON result = %+v, error = %v", result, err)
	}
}

func fixtureExecutor(
	t *testing.T,
	run func(context.Context, fixtureInput) (fixtureOutput, error),
) tool.Executor {
	t.Helper()
	executor, err := Define(fixtureSpec(run))
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func fixtureSpec(
	run func(context.Context, fixtureInput) (fixtureOutput, error),
) Spec[fixtureInput, fixtureOutput] {
	descriptor := tool.Descriptor{
		Name: "typed_fixture", Description: "Typed fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "fixture", ID: "value", Access: tool.AccessRead,
		}}},
		Availability: tool.AvailabilityAvailable,
		RepeatPolicy: tool.RepeatExecute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "minLength": 1},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	}
	return Spec[fixtureInput, fixtureOutput]{
		Descriptor:  descriptor,
		Disposition: tool.DispositionAbortImmediately,
		Run:         run,
		Metadata: func(output fixtureOutput) map[string]any {
			return map[string]any{"greeting": output.Greeting}
		},
		Encode: func(output fixtureOutput) (tool.Result, error) {
			return toolresult.Success(output, nil)
		},
	}
}
