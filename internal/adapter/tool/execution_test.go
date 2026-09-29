package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestResolveBoundRefFreezesCatalogAuthority(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &executionFixture{name: "authority_fixture"}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := snapshot.Binding(executor.name)
	if !ok {
		t.Fatal("fixture binding is missing")
	}
	ref, descriptor, resolved, err := registry.ResolveBoundRef(executor.name, binding)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != executor.name ||
		ref.Source == "" ||
		ref.CatalogID != snapshot.CatalogID ||
		ref.Generation != snapshot.Generation ||
		ref.Revision != binding.Revision ||
		ref.Authority != binding.Authority ||
		descriptor.Name != executor.name ||
		resolved == nil {
		t.Fatalf("resolved reference = %+v descriptor=%+v", ref, descriptor)
	}
	if ref.Binding() != binding {
		t.Fatalf("round-trip binding = %+v, want %+v", ref.Binding(), binding)
	}
}

func TestExecuteWithOutcomeNormalizesLifecycle(t *testing.T) {
	executor := &executionFixture{name: "outcome_fixture"}
	result, outcome, err := tool.ExecuteWithOutcome(
		t.Context(), executor, json.RawMessage(`{}`),
	)
	if err != nil || result.Content != "ok" ||
		outcome.Status != tool.OutcomeSucceeded ||
		result.Outcome == nil || result.Outcome.Status != outcome.Status {
		t.Fatalf("result = %+v, outcome = %+v, error = %v", result, outcome, err)
	}
	_, outcome, err = tool.ExecuteWithOutcome(
		t.Context(), structuredExecutionFixture{}, json.RawMessage(`{}`),
	)
	if err != nil || outcome.Security == nil ||
		outcome.Security.EgressDenied == nil ||
		outcome.Security.EgressDenied.Host != "example.com" {
		t.Fatalf("structured outcome = %+v, error = %v", outcome, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, outcome, err := tool.ExecuteWithOutcome(
		ctx, executor, json.RawMessage(`{}`),
	); !errors.Is(err, context.Canceled) || outcome.Status != tool.OutcomeCanceled {
		t.Fatalf("canceled outcome = %+v, error = %v", outcome, err)
	}
	if _, outcome, err := tool.ExecuteWithOutcome(
		t.Context(), panickingExecutionFixture{}, json.RawMessage(`{}`),
	); err == nil || outcome.Status != tool.OutcomeFailed ||
		!strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic outcome = %+v, error = %v", outcome, err)
	}
}

func TestResultStorePreservesTypedOutcomeAndExecutionReceipt(t *testing.T) {
	store := tool.NewResultStore(32)
	result := tool.Result{
		Content: strings.Repeat("x", 256),
		Outcome: &tool.Outcome{
			Status: tool.OutcomeFailed,
			Security: &tool.SecuritySignal{
				EgressDenied: &tool.NetworkTarget{Host: "example.com", Protocol: "https"},
			},
		},
		Execution: &tool.ExecutionReceipt{
			Tool: tool.ToolRef{
				Name: "fixture", Source: "builtin:fixture",
				CatalogID: "catalog-1", Generation: 1, Revision: 1, Authority: 9,
			},
			Source: tool.InvocationSourceModel, Disposition: tool.DispositionWaitForTeardown,
			Attempts: []tool.AttemptReceipt{{
				Sequence: 1, Sandbox: "strong", Status: tool.OutcomeFailed,
			}},
		},
	}
	routed := store.RouteFor("fixture", result)
	if !routed.Truncated || routed.Handle == "" ||
		routed.Outcome == nil || routed.Outcome.Security == nil ||
		routed.Outcome.Security.EgressDenied == nil ||
		routed.Execution == nil || len(routed.Execution.Attempts) != 1 {
		t.Fatalf("routed result = %+v", routed)
	}
}

func TestCloneExecutionReceiptDeepCopiesAuthorityEvidence(t *testing.T) {
	source := &tool.ExecutionReceipt{
		Attempts: []tool.AttemptReceipt{{
			ReadRoots:      []string{"/workspace"},
			WritePaths:     []string{"/workspace/result.txt"},
			NetworkTargets: []string{"https://example.com:443"},
			Provenance: []tool.PermissionProvenance{{
				Kind: "grant", Value: "managed", Digest: "grant-digest",
			}},
			Denial: &sandbox.Denial{
				Operation: sandbox.DenialWrite,
				Resource:  "/workspace/result.txt", ReasonCode: "denied",
			},
			Amendment: &tool.PermissionAmendmentReceipt{
				BasePermissionDigest: "base", Kind: "path_write",
				Resource: "/workspace/result.txt", Decision: "approved",
			},
		}},
	}
	cloned := tool.CloneExecutionReceipt(source)
	source.Attempts[0].ReadRoots[0] = "/tampered"
	source.Attempts[0].WritePaths[0] = "/tampered"
	source.Attempts[0].NetworkTargets[0] = "https://tampered:443"
	source.Attempts[0].Provenance[0].Digest = "tampered"
	source.Attempts[0].Denial.Resource = "/tampered"
	source.Attempts[0].Amendment.Resource = "/tampered"
	attempt := cloned.Attempts[0]
	if attempt.ReadRoots[0] != "/workspace" ||
		attempt.WritePaths[0] != "/workspace/result.txt" ||
		attempt.NetworkTargets[0] != "https://example.com:443" ||
		attempt.Provenance[0].Digest != "grant-digest" ||
		attempt.Denial.Resource != "/workspace/result.txt" ||
		attempt.Amendment.Resource != "/workspace/result.txt" {
		t.Fatalf("cloned authority evidence was mutated: %+v", attempt)
	}
}

func TestExecutionReceiptAuthorityEvidenceRoundTripsJSON(t *testing.T) {
	source := tool.ExecutionReceipt{Attempts: []tool.AttemptReceipt{{
		PermissionSchemaVersion: 2,
		PermissionRevision:      2,
		PermissionDigest:        strings.Repeat("a", 64),
		Enforcement:             "strong",
		Backend:                 "seatbelt",
		ReadRoots:               []string{"/workspace"},
		NetworkMode:             "proxy_targets",
		Provenance: []tool.PermissionProvenance{{
			Kind: "policy", Value: "snapshot", Revision: 7,
		}},
		Amendment: &tool.PermissionAmendmentReceipt{
			BasePermissionDigest: strings.Repeat("b", 64),
			Kind:                 "path_read",
			Resource:             "/workspace/input.txt",
			Decision:             "approved",
			AmendedPermissionDigest: strings.Repeat(
				"a",
				64,
			),
		},
	}}}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var decoded tool.ExecutionReceipt
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	attempt := decoded.Attempts[0]
	if attempt.PermissionRevision != 2 ||
		attempt.PermissionDigest != strings.Repeat("a", 64) ||
		attempt.Enforcement != "strong" ||
		attempt.Backend != "seatbelt" ||
		attempt.NetworkMode != "proxy_targets" ||
		attempt.Provenance[0].Revision != 7 ||
		attempt.Amendment.AmendedPermissionDigest != strings.Repeat("a", 64) {
		t.Fatalf("round-tripped authority evidence = %+v", attempt)
	}
}

func TestExecutionAdmissionComesFromContext(t *testing.T) {
	var admitted, released bool
	ctx := tool.WithExecutionAdmission(
		context.Background(),
		func(_ context.Context, policy tool.ParallelPolicy) (func(), error) {
			admitted = policy == tool.ParallelConcurrent
			return func() { released = true }, nil
		},
	)
	release, err := tool.AdmitExecution(ctx, tool.ParallelConcurrent)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !admitted || !released {
		t.Fatalf("admitted=%t released=%t", admitted, released)
	}
}

func TestOutcomeFactsCloneWithoutMetadataAuthority(t *testing.T) {
	result := tool.Result{Outcome: &tool.Outcome{
		Status: tool.OutcomeSucceeded,
		Facts: &tool.OutcomeFacts{
			WorkspaceRead: &tool.WorkspaceReadFact{
				Path: "a.go", Digest: "sha256:a",
			},
			WorkspaceChanges: []tool.WorkspaceChange{{
				Path: "a.go", Kind: tool.WorkspaceModified, Added: 1,
			}},
			Evidence: []tool.EvidenceHit{{
				Kind: tool.EvidenceDefinition, Path: "a.go",
			}},
			Completion: &tool.CompletionDeclaration{
				Status: "complete", ChangedPaths: []string{"a.go"},
			},
		},
	}}
	cloned := tool.CloneOutcome(result.Outcome)
	cloned.Facts.WorkspaceRead.Path = "b.go"
	cloned.Facts.WorkspaceChanges[0].Path = "b.go"
	cloned.Facts.Evidence[0].Path = "b.go"
	cloned.Facts.Completion.ChangedPaths[0] = "b.go"
	if result.Outcome.Facts.WorkspaceRead.Path != "a.go" ||
		result.Outcome.Facts.WorkspaceChanges[0].Path != "a.go" ||
		result.Outcome.Facts.Evidence[0].Path != "a.go" ||
		result.Outcome.Facts.Completion.ChangedPaths[0] != "a.go" {
		t.Fatalf("Outcome Facts clone aliased source: %+v", result.Outcome.Facts)
	}
}

type executionFixture struct{ name string }

func (e *executionFixture) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: e.name, Description: "execution fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{}, "additionalProperties": false,
		},
	}
}

func (*executionFixture) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

type panickingExecutionFixture struct{}

func (panickingExecutionFixture) Descriptor() tool.Descriptor {
	return (&executionFixture{name: "panic_fixture"}).Descriptor()
}

func (panickingExecutionFixture) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	panic("boom")
}

type structuredExecutionFixture struct{}

func (structuredExecutionFixture) Descriptor() tool.Descriptor {
	return (&executionFixture{name: "structured_fixture"}).Descriptor()
}

func (structuredExecutionFixture) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{
		IsError: true,
		Outcome: &tool.Outcome{
			Status: tool.OutcomeFailed,
			Security: &tool.SecuritySignal{
				EgressDenied: &tool.NetworkTarget{
					Host: "example.com", Protocol: "https",
				},
			},
		},
	}, nil
}
