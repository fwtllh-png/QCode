package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

type contractInput struct {
	Value string `json:"value"`
}

type contractOutput struct {
	Value string `json:"value"`
}

func TestExecutorContract(t *testing.T) {
	executor := contractExecutor(t, func(
		_ context.Context,
		input contractInput,
	) (contractOutput, error) {
		return contractOutput{Value: input.Value}, nil
	}, nil)
	if err := tool.ValidateDescriptor(executor.Descriptor()); err != nil {
		t.Fatalf("descriptor: %v", err)
	}

	registry := tool.NewRegistry(nil, tool.NewResultStore(16))
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"value":"ok","unknown":true}`} {
		_, err := tooltest.Execute(t.Context(), registry, tool.Call{
			Name: "contract_fixture", Arguments: json.RawMessage(raw),
		})
		if !errors.Is(err, tool.ErrInvalidArguments) ||
			tool.ErrorCategory(err) != tool.ErrorCategoryInvalidArguments {
			t.Fatalf("schema error for %s = %v", raw, err)
		}
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "contract_fixture",
		Arguments: json.RawMessage(`{"value":"` + strings.Repeat("x", 32) + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.Handle == "" || result.OriginalBytes <= 16 {
		t.Fatalf("output routing = %+v", result)
	}

	guard, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		), Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(
		t.Context(),
		"contract-call",
		"contract_fixture",
		json.RawMessage(`{"value":"guarded"}`),
	); err != nil {
		t.Fatalf("guard path: %v", err)
	}

	panicExecutor := contractExecutor(t, func(
		context.Context,
		contractInput,
	) (contractOutput, error) {
		panic("contract panic")
	}, nil)
	if _, err := panicExecutor.Execute(
		t.Context(),
		json.RawMessage(`{"value":"panic"}`),
	); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic containment error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executor.Execute(
		ctx,
		json.RawMessage(`{"value":"cancel"}`),
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	invalidMetadata := contractExecutor(t, func(
		_ context.Context,
		input contractInput,
	) (contractOutput, error) {
		return contractOutput{Value: input.Value}, nil
	}, func(contractOutput) map[string]any {
		return map[string]any{"invalid": make(chan struct{})}
	})
	if _, err := invalidMetadata.Execute(
		t.Context(),
		json.RawMessage(`{"value":"metadata"}`),
	); err == nil {
		t.Fatal("non-JSON metadata succeeded")
	}

	deferredRegistry := tool.NewRegistry(nil, nil)
	if _, err := deferredRegistry.Reconcile(
		"contract",
		0,
		[]tool.Registration{tool.NewExternalDeferredRegistration(
			tool.ExternalFromDescriptor(executor.Descriptor()),
			tool.TrustedBindingFromDescriptor(executor.Descriptor()),
			func() (tool.Executor, error) { return executor, nil },
		)},
	); err != nil {
		t.Fatal(err)
	}
	before, err := deferredRegistry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	beforeEntry, ok := before.Lookup("contract_fixture")
	if !ok || beforeEntry.State != tool.CatalogEntryDeferred {
		t.Fatalf("deferred entry = %+v, found = %t", beforeEntry, ok)
	}
	if _, err := deferredRegistry.Materialize(
		beforeEntry.Name,
		beforeEntry.Revision,
	); err != nil {
		t.Fatal(err)
	}
	after, err := deferredRegistry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	afterEntry, ok := after.Lookup("contract_fixture")
	if !ok || afterEntry.State != tool.CatalogEntryMaterialized ||
		afterEntry.Name != beforeEntry.Name ||
		afterEntry.Source != beforeEntry.Source ||
		afterEntry.Revision != beforeEntry.Revision+1 ||
		tool.CatalogToolID(afterEntry.Name, afterEntry.Source) !=
			tool.CatalogToolID(beforeEntry.Name, beforeEntry.Source) {
		t.Fatalf(
			"catalog identity changed across materialization: before=%+v after=%+v",
			beforeEntry,
			afterEntry,
		)
	}
}

func contractExecutor(
	t *testing.T,
	run func(context.Context, contractInput) (contractOutput, error),
	metadata func(contractOutput) map[string]any,
) tool.Executor {
	t.Helper()
	descriptor := tool.Descriptor{
		Name:        "contract_fixture",
		Description: "Exercise the shared executor contract",
		Visibility:  tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		ResourceResolver:   tool.ResourceResolver{},
		Availability:       tool.AvailabilityAvailable,
		RepeatPolicy:       tool.RepeatExecute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string", "minLength": 1},
			},
			"required":             []string{"value"},
			"additionalProperties": false,
		},
	}
	executor, err := typed.Define(typed.Spec[contractInput, contractOutput]{
		Descriptor:  descriptor,
		Disposition: tool.DispositionAbortImmediately,
		Run:         run,
		Metadata:    metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}
