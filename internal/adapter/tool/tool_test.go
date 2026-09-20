package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adaptercontent "github.com/fwtllh-png/QCode/internal/adapter/content"
)

func executeRegistry(
	ctx context.Context,
	registry *Registry,
	call Call,
) (Result, error) {
	name, descriptor, executor, err := registry.Resolve(call.Name)
	if err != nil {
		return Result{}, err
	}
	arguments := RepairArguments(call.Arguments)
	if err := ValidateArguments(descriptor.InputSchema, arguments); err != nil {
		return Result{}, fmt.Errorf("tool %q arguments: %w", name, err)
	}
	result, _, err := registry.ExecutePreparedOutcome(
		ctx,
		name,
		arguments,
		executor,
	)
	return result, err
}

func TestRegistryTestExecutionValidatesArguments(t *testing.T) {
	executor := &countingTool{}
	registry := NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	for _, call := range []Call{
		{Name: "count", Arguments: json.RawMessage(`{"value":1}`)},
	} {
		if _, err := executeRegistry(t.Context(), registry, call); err == nil {
			t.Fatalf("Execute(%s) error = nil", call.Arguments)
		}
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls.Load())
	}
	if _, err := executeRegistry(t.Context(), registry, Call{
		Name: "count", Arguments: json.RawMessage(`{"value":"ok"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls.Load())
	}
}

func TestRegistryRejectsConflictDeterministically(t *testing.T) {
	registry := NewRegistry(nil, nil)
	if err := registry.Register(&countingTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&countingTool{}); err == nil || err.Error() != `tool "count" is already registered` {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestRegistryRepairsFencedJSONArguments(t *testing.T) {
	executor := &countingTool{}
	registry := NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRegistry(t.Context(), registry, Call{
		Name: "count", Arguments: json.RawMessage("```json\n{\"value\":\"ok\"}\n```"),
	}); err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls = %d", executor.calls.Load())
	}
}

func TestRecoveryHintSurvivesPreconditionWrapping(t *testing.T) {
	err := Precondition(WithRecoveryHint(errors.New("stale edit"), RecoveryHint{
		ErrorCategory:  "edit_precondition_failed",
		RequiredAction: "file_read",
		Path:           "docs/chapter.md",
		RetryOriginal:  false,
	}))

	hint, ok := RecoveryHintFromError(fmt.Errorf("plan workspace edit: %w", err))

	if !ok || hint.ErrorCategory != "edit_precondition_failed" ||
		hint.RequiredAction != "file_read" ||
		hint.Path != "docs/chapter.md" ||
		hint.RetryOriginal {
		t.Fatalf("hint = %+v, found = %v", hint, ok)
	}
}

func TestResultStoreReturnsTruncationHandle(t *testing.T) {
	store := NewResultStore(4)
	result := store.Route(Result{Content: "abcdefgh"})
	if !result.Truncated || result.OriginalBytes != 8 || result.Handle == "" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "Warning: truncated output") {
		t.Fatalf("missing truncation warning: %q", result.Content)
	}
	if !strings.Contains(result.Content, result.Handle) {
		t.Fatalf("result = %+v", result)
	}
	if full, ok := store.Get(result.Handle); !ok || full != "abcdefgh" {
		t.Fatalf("stored result = %q, %t", full, ok)
	}
}

func TestResultStoreAppliesCallerTokenBudgetAndKeepsTypedHandle(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := strings.Repeat("build output line\n", 7000)
	for _, test := range []struct {
		name string
		kind string
	}{
		{name: "file_read", kind: "read"},
		{name: "write_stdin", kind: "build"},
		{name: "exec_command", kind: "build"},
		{name: "custom_tool", kind: "generic"},
	} {
		result, receipt := store.AdmitWithin(
			test.name,
			Result{Content: payload},
			2_048,
		)
		if !result.Truncated || result.Handle == "" ||
			receipt.TokenLimit != 2_048 ||
			receipt.RetainedTokens > receipt.TokenLimit ||
			len(result.Content)*10 > len(payload)*3 ||
			result.Metadata["projection_kind"] != test.kind {
			t.Fatalf("%s projection = %+v bytes=%d", test.name, result, len(result.Content))
		}
		if full, ok := store.Get(result.Handle); !ok || full != payload {
			t.Fatalf("%s full handle bytes=%d found=%t", test.name, len(full), ok)
		}
	}
}

func TestTurnHistoryAdmissionProjectsTailConclusions(t *testing.T) {
	store := NewResultStore(512)
	payload := "HEAD-AUDIT-START\n" + strings.Repeat("explore the lexer\n", 200) +
		"[turn 1 assistant] five P2s: missing overflow test\n"
	admitted, receipt := store.Admit("turn_history", Result{Content: payload})
	if !admitted.Truncated || admitted.Handle == "" ||
		receipt.Kind != "turn_history" ||
		!strings.Contains(admitted.Content, "five P2s: missing overflow test") ||
		strings.Contains(admitted.Content, "HEAD-AUDIT-START") ||
		!strings.Contains(admitted.Content, `mode="tail"`) ||
		!strings.Contains(admitted.Content, `mode="query"`) {
		t.Fatalf("admitted=%+v receipt=%+v", admitted, receipt)
	}
	full, ok := store.Get(admitted.Handle)
	if !ok || full != payload {
		t.Fatalf("stored = %q found=%t", full, ok)
	}
}

func TestResultAdmissionShrinksHundredKiBAndRetainsOriginalByHandle(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := strings.Repeat("0123456789abcdef", 6400)
	admitted, receipt := store.Admit("exec_command", Result{Content: payload})
	if !admitted.Truncated || admitted.Handle == "" ||
		receipt.Handle != admitted.Handle ||
		receipt.OriginalBytes != 100<<10 ||
		receipt.RetainedBytes != len(admitted.Content) ||
		receipt.RetainedBytes > 32<<10 ||
		// ASCII inline bytes stay under the dense-script token ceiling.
		receipt.RetainedTokens > store.TokenCapacity() ||
		receipt.Reason != "token_limit" ||
		receipt.Digest == "" {
		t.Fatalf("admitted=%+v receipt=%+v", admitted, receipt)
	}
	full, ok := store.Get(receipt.Handle)
	if !ok || full != payload {
		t.Fatalf("full bytes=%d found=%t", len(full), ok)
	}
	again, secondReceipt := store.Admit("exec_command", admitted)
	if again.Handle != admitted.Handle ||
		secondReceipt.Digest != receipt.Digest ||
		secondReceipt.OriginalBytes != receipt.OriginalBytes ||
		secondReceipt.RetainedBytes != receipt.RetainedBytes {
		t.Fatalf("idempotent admission=%+v receipt=%+v", again, secondReceipt)
	}
	if projected := ModelResult("exec_command", admitted); projected.Admission != nil {
		t.Fatalf("model result leaked admission receipt: %+v", projected)
	}
}

func TestResultAdmissionRejectsForgedReceipt(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := strings.Repeat("x", 100<<10)
	admitted, receipt := store.Admit("exec_command", Result{
		Content: payload,
		Admission: &adaptercontent.AdmissionReceipt{
			Kind: "build", Reason: "inline", Digest: "sha256:forged",
			OriginalBytes: 1, RetainedBytes: len(payload),
			OriginalTokens: 1, RetainedTokens: 1, TokenLimit: 10_000,
		},
	})
	if !admitted.Truncated || receipt.Handle == "" ||
		receipt.Digest == "sha256:forged" ||
		receipt.RetainedTokens > 10_000 {
		t.Fatalf("forged receipt accepted: %+v %+v", admitted, receipt)
	}
}

func TestResultStorePrunesContextSurfaceWithHeadTailAndStableHandle(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := "HEAD-" + strings.Repeat("middle", 2000) + "-TAIL"
	input := Result{
		Content:  payload,
		Metadata: map[string]any{"error_category": "fixture"},
	}
	first, changed := store.PruneSurface("file_read", input, 1024)
	if !changed ||
		!first.Truncated ||
		first.Handle == "" ||
		first.OriginalBytes != len(payload) ||
		!strings.Contains(first.Content, "HEAD-") ||
		!strings.Contains(first.Content, "-TAIL") ||
		!strings.Contains(first.Content, "... pruned middle ...") {
		t.Fatalf("first projection = %+v", first)
	}
	full, ok := store.Get(first.Handle)
	if !ok || full != payload {
		t.Fatalf("stored result bytes=%d found=%t", len(full), ok)
	}
	second, changed := store.PruneSurface("file_read", first, 512)
	if !changed || second.Handle != first.Handle ||
		!strings.Contains(second.Content, "HEAD-") ||
		!strings.Contains(second.Content, "-TAIL") {
		t.Fatalf("second projection = %+v", second)
	}
	if skipped, changed := store.PruneSurface("result_get", input, 512); changed ||
		skipped.Content != payload {
		t.Fatalf("retrieval projection changed = %+v, %t", skipped, changed)
	}
}

func TestResultGetPagesReconstructFullLargeResult(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := strings.Repeat("0123456789abcdef", 7000)
	routed := store.RouteFor("exec_command", Result{Content: payload})
	registry := NewRegistry(nil, store)
	var reconstructed strings.Builder
	offset := 0
	for {
		raw, _ := json.Marshal(map[string]any{
			"handle": routed.Handle, "mode": "bytes",
			"offset": offset, "max_bytes": 32 << 10,
		})
		page, err := executeRegistry(t.Context(), registry, Call{
			Name: "result_get", Arguments: raw,
		})

		if err != nil {
			t.Fatal(err)
		}
		page, _ = store.Admit("result_get", page)
		reconstructed.WriteString(page.Content)
		next, more := page.Metadata["next_offset"]
		if !more {
			break
		}
		offset = next.(int)
	}
	if reconstructed.String() != payload {
		t.Fatalf("reconstructed bytes = %d, want %d", reconstructed.Len(), len(payload))
	}
}

func TestResultGetMissingHandleReturnsStructuredPrecondition(t *testing.T) {
	registry := NewRegistry(nil, NewResultStore(32<<10))
	_, err := executeRegistry(t.Context(), registry, Call{
		Name:      "result_get",
		Arguments: json.RawMessage(`{"handle":"call-id-is-not-a-handle"}`),
	})

	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("Execute() error = %v, want precondition", err)
	}
	hint, ok := RecoveryHintFromError(err)
	if !ok ||
		hint.ErrorCategory != "result_handle_not_found" ||
		hint.RequiredAction != "use_advertised_result_handle" ||
		hint.RetryOriginal {
		t.Fatalf("recovery hint = %+v, found = %t", hint, ok)
	}
}

func TestModelResultRetainsOnlyModelMetadata(t *testing.T) {
	input := Result{Content: "ok", Metadata: map[string]any{
		"error_category":  "retryable",
		"required_action": "file_read",
		"canonical_path":  "/private/workspace/a.go",
		"fingerprint":     map[string]any{"runtime_only": true},
	}, Outcome: &Outcome{
		Status: OutcomeSucceeded,
		Facts:  &OutcomeFacts{ResultHandle: "internal-handle"},
	}, Execution: &ExecutionReceipt{}, Admission: &adaptercontent.AdmissionReceipt{}}
	projected := ModelResult("file_write", input)
	if len(projected.Metadata) != 2 ||
		projected.Metadata["error_category"] != "retryable" ||
		projected.Metadata["required_action"] != "file_read" ||
		projected.Outcome != nil || projected.Execution != nil ||
		projected.Admission != nil {
		t.Fatalf("model result = %#v", projected)
	}
	retrieved := ModelResult("result_get", input)
	if len(retrieved.Metadata) != len(input.Metadata) ||
		retrieved.Outcome != nil || retrieved.Execution != nil ||
		retrieved.Admission != nil {
		t.Fatalf("retrieval result = %#v", retrieved)
	}
}

func TestModelResultProcessProjectionIsIdempotent(t *testing.T) {
	input := Result{
		Content:  "partial output",
		Metadata: map[string]any{"description": "internal"},
		Outcome: &Outcome{
			Status: OutcomeSucceeded,
			Facts: &OutcomeFacts{ProcessSession: &ProcessSessionFact{
				SessionID: "session-1",
				Cursor:    42,
				Running:   true,
			}},
		},
	}
	first := ModelResult("exec_command", input)
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var restored Result
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	second := ModelResult("exec_command", restored)
	if second.Metadata["session_id"] != "session-1" ||
		second.Metadata["cursor"] != float64(42) ||
		second.Metadata["running"] != true ||
		second.Outcome != nil {
		t.Fatalf("second projection = %#v", second)
	}
}

func TestResultRetrievalModesAreBoundedAndPagePastInlineContent(t *testing.T) {
	const content = "line1\nline2\nneedle-three\nline4\nomega"
	store := NewResultStore(16)
	routed := store.Route(Result{
		Content: content, IsError: true, Metadata: map[string]any{"source": "fixture"},
	})
	registry := NewRegistry(nil, store)

	tests := []struct {
		name      string
		arguments string
		contains  string
	}{
		{name: "metadata", arguments: `{"mode":"metadata"}`},
		{name: "summary", arguments: `{"mode":"summary"}`, contains: "..."},
		{name: "head", arguments: `{"mode":"head"}`, contains: "line1"},
		{name: "tail", arguments: `{"mode":"tail"}`, contains: "omega"},
		{name: "lines", arguments: `{"mode":"lines","start_line":3,"max_lines":1}`, contains: "needle-three"},
		{name: "query", arguments: `{"mode":"query","query":"needle"}`, contains: "needle-three"},
		{name: "bytes", arguments: `{"mode":"bytes","offset":25}`, contains: "line4"},
		{name: "full alias", arguments: `{"mode":"full"}`, contains: "line1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var arguments map[string]any
			if err := json.Unmarshal([]byte(test.arguments), &arguments); err != nil {
				t.Fatal(err)
			}
			arguments["handle"] = routed.Handle
			raw, err := json.Marshal(arguments)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executeRegistry(t.Context(), registry, Call{
				Name: "result_get", Arguments: raw,
			})

			if err != nil {
				t.Fatal(err)
			}
			if len(result.Content) > 16 {
				t.Fatalf("content bytes = %d, want <= 16: %q", len(result.Content), result.Content)
			}
			if !strings.Contains(result.Content, test.contains) {
				t.Fatalf("content = %q, want substring %q", result.Content, test.contains)
			}
			if result.OriginalBytes != len(content) || result.Metadata["total_bytes"] != len(content) {
				t.Fatalf("result size metadata = %+v", result)
			}
			if result.Metadata["source"] != "fixture" || !result.IsError {
				t.Fatalf("stored result fields were not preserved: %+v", result)
			}
		})
	}
}

func TestResultRetrievalCannotBypassHardCap(t *testing.T) {
	store := NewResultStore(8)
	routed := store.Route(Result{Content: "0123456789abcdefghijklmnopqrstuvwxyz"})
	registry := NewRegistry(nil, store)
	for _, arguments := range []string{
		`{"mode":"head","max_bytes":999999}`,
		`{"mode":"bytes","offset":1,"max_bytes":999999}`,
		`{"mode":"lines","start_line":1,"max_lines":999999,"max_bytes":999999}`,
		`{"mode":"query","query":"0","max_lines":999999,"max_bytes":999999}`,
	} {
		var input map[string]any
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			t.Fatal(err)
		}
		input["handle"] = routed.Handle
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := executeRegistry(t.Context(), registry, Call{
			Name: "result_get", Arguments: raw,
		})

		if err != nil {
			t.Fatal(err)
		}
		if len(result.Content) > 8 {
			t.Fatalf("result_get(%s) returned %d bytes", arguments, len(result.Content))
		}
	}
}

func TestClaimsWaitAndCancellationRelease(t *testing.T) {
	claims := NewClaims()
	release, err := claims.Acquire(t.Context(), []string{"file:a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := claims.Acquire(ctx, []string{"file:a"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v", err)
	}
	release()
	if claims.Active() != 0 {
		t.Fatalf("active claims = %d", claims.Active())
	}
}

func TestHierarchicalClaimsAllowReadReadAndBlockWriteTreeOverlap(t *testing.T) {
	claims := NewClaims()
	root := t.TempDir()
	parent := Resource{Kind: "directory", Path: root, Access: AccessRead, Tree: true}
	child := Resource{
		Kind: "file", Path: filepath.Join(root, "child.txt"), Access: AccessRead,
	}
	releaseParent, err := claims.AcquireResources(t.Context(), []Resource{parent})
	if err != nil {
		t.Fatal(err)
	}
	releaseChild, err := claims.AcquireResources(t.Context(), []Resource{child})
	if err != nil {
		t.Fatalf("read/read unexpectedly conflicted: %v", err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, err := claims.AcquireResources(context.Background(), []Resource{{
			Kind: "file", Path: child.Path, Access: AccessWrite,
		}})
		if err == nil {
			acquired <- release
		}
	}()
	select {
	case <-acquired:
		t.Fatal("write acquired while overlapping tree reads were held")
	default:
	}
	releaseChild()
	select {
	case <-acquired:
		t.Fatal("write acquired while parent tree read was held")
	default:
	}
	releaseParent()
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("write did not acquire after overlapping reads released")
	}
}

func TestHierarchicalClaimsCanonicalTargetConflict(t *testing.T) {
	claims := NewClaims()
	target := filepath.Join(t.TempDir(), "target.txt")
	release, err := claims.AcquireResources(t.Context(), []Resource{{
		Kind: "file", Path: target, Access: AccessWrite,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = claims.AcquireResources(ctx, []Resource{{
		Kind: "file", Path: filepath.Clean(target), Access: AccessWrite,
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canonical target conflict error = %v", err)
	}
	release()
}

func TestClaimsPreserveConflictOrderWithoutBlockingDisjointWork(t *testing.T) {
	claims := NewClaims()
	resourceA := Resource{
		Kind: "file", Path: filepath.Join(t.TempDir(), "a"), Access: AccessRead,
	}
	releaseReader, err := claims.AcquireResources(t.Context(), []Resource{resourceA})
	if err != nil {
		t.Fatal(err)
	}
	writerReady := make(chan func(), 1)
	go func() {
		write := resourceA
		write.Access = AccessWrite
		release, acquireErr := claims.AcquireResources(t.Context(), []Resource{write})
		if acquireErr == nil {
			writerReady <- release
		}
	}()
	for claims.Waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	lateReaderReady := make(chan func(), 1)
	go func() {
		release, acquireErr := claims.AcquireResources(t.Context(), []Resource{resourceA})
		if acquireErr == nil {
			lateReaderReady <- release
		}
	}()
	for claims.Waiting() != 2 {
		time.Sleep(time.Millisecond)
	}
	resourceB := Resource{
		Kind: "file", Path: filepath.Join(t.TempDir(), "b"), Access: AccessWrite,
	}
	releaseB, err := claims.AcquireResources(t.Context(), []Resource{resourceB})
	if err != nil {
		t.Fatalf("disjoint Claim was blocked: %v", err)
	}
	releaseB()

	releaseReader()
	var releaseWriter func()
	select {
	case releaseWriter = <-writerReady:
	case <-time.After(time.Second):
		t.Fatal("queued writer starved behind readers")
	}
	select {
	case release := <-lateReaderReady:
		release()
		t.Fatal("later reader bypassed an earlier conflicting writer")
	default:
	}
	releaseWriter()
	select {
	case release := <-lateReaderReady:
		release()
	case <-time.After(time.Second):
		t.Fatal("reader did not acquire after writer released")
	}
	if claims.Active() != 0 || claims.Waiting() != 0 {
		t.Fatalf("Claims leaked active=%d waiting=%d", claims.Active(), claims.Waiting())
	}
}

type countingTool struct {
	calls atomic.Int32
}

func (*countingTool) Descriptor() Descriptor {
	return Descriptor{
		Name: "count", Description: "count calls", Visibility: VisibleModel,
		Capability: CapabilityRead, AccessMode: AccessRead,
		ParallelPolicy: ParallelConcurrent, SandboxRequirement: SandboxNone,
		Availability: AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
			"required":             []string{"value"},
			"additionalProperties": false,
		},
	}
}

func (t *countingTool) Execute(context.Context, json.RawMessage) (Result, error) {
	t.calls.Add(1)
	return Result{Content: "ok"}, nil
}

type closableTool struct {
	closed atomic.Int32
}

func (*closableTool) Descriptor() Descriptor {
	return Descriptor{
		Name: "closable", Description: "closable fixture", Visibility: VisibleModel,
		Capability: CapabilityRead, AccessMode: AccessRead,
		ParallelPolicy: ParallelConcurrent, SandboxRequirement: SandboxNone,
		Availability: AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
		},
	}
}

func (*closableTool) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{Content: "ok"}, nil
}

func (t *closableTool) Close() error {
	t.closed.Add(1)
	return nil
}

func TestRegistryCloseReleasesExecutorResources(t *testing.T) {
	registry := NewRegistry(nil, nil)
	instance := &closableTool{}
	if err := registry.Register(instance); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if instance.closed.Load() != 1 {
		t.Fatalf("close calls = %d, want 1", instance.closed.Load())
	}
}

func TestModelResultRetainsEditRecoveryFacts(t *testing.T) {
	input := Result{Content: "failed", IsError: true, Metadata: map[string]any{
		"error_category":  "edit_precondition_miss",
		"required_action": "file_read",
		"failed_change":   1,
		"match_count":     0,
		"start_line":      74,
		"end_line":        80,
		"current_excerpt": "actual current text",
		"canonical_path":  "/private/workspace/a.go",
	}}
	projected := ModelResult("file_apply", input)
	for _, key := range []string{
		"failed_change", "match_count", "start_line", "end_line",
		"current_excerpt",
	} {
		if _, ok := projected.Metadata[key]; !ok {
			t.Fatalf("projected metadata lost %q: %#v", key, projected.Metadata)
		}
	}
	if _, ok := projected.Metadata["canonical_path"]; ok {
		t.Fatalf("projected metadata retained runtime-only key: %#v", projected.Metadata)
	}
}

func TestValidateArgumentsCachesCompiledSchema(t *testing.T) {
	integerSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"value": map[string]any{"type": "integer"}},
		"required":             []any{"value"},
		"additionalProperties": false,
	}
	for pass := 0; pass < 2; pass++ {
		if err := ValidateArguments(integerSchema, json.RawMessage(`{"value":1}`)); err != nil {
			t.Fatalf("pass %d: valid arguments rejected: %v", pass, err)
		}
		if err := ValidateArguments(integerSchema, json.RawMessage(`{"value":"no"}`)); err == nil {
			t.Fatalf("pass %d: invalid arguments accepted", pass)
		}
	}
	stringSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"value": map[string]any{"type": "string"}},
		"required":             []any{"value"},
		"additionalProperties": false,
	}
	if err := ValidateArguments(stringSchema, json.RawMessage(`{"value":1}`)); err == nil {
		t.Fatal("integer accepted against string schema")
	}
	if err := ValidateArguments(stringSchema, json.RawMessage(`{"value":"ok"}`)); err != nil {
		t.Fatalf("string rejected against string schema: %v", err)
	}
	if err := ValidateArguments(integerSchema, json.RawMessage(`{"value":"ok"}`)); err == nil {
		t.Fatal("string accepted against integer schema after cache reuse")
	}
}

func batchResultContent(units int) string {
	return strings.Repeat("a", units*4)
}

func TestAdmitBatchWithinRedistributesUnusedShares(t *testing.T) {
	store := NewResultStore(32 << 10)
	names := []string{"file_read", "file_read", "file_read", "file_read"}
	results := []Result{
		{Content: batchResultContent(100)},
		{Content: batchResultContent(200)},
		{Content: batchResultContent(300)},
		{Content: batchResultContent(8000)},
	}
	// The equal split of the 8000-unit pool would cap every result at 2000;
	// the pool instead admits the small results whole and hands their
	// reclaimed shares to the large one (100+200+300 inline, 7400 left).
	admitted := store.AdmitBatchWithin(names, results, 8000, 8000)
	for _, index := range []int{0, 1, 2} {
		if admitted[index].Truncated {
			t.Fatalf("small result %d truncated: %+v",
				index, admitted[index].Admission)
		}
	}
	large := admitted[3]
	if !large.Truncated || large.Handle == "" {
		t.Fatalf("large result not spilled: %+v", large)
	}
	if large.Admission.TokenLimit != 7400 {
		t.Fatalf("large allocation = %d, want 7400",
			large.Admission.TokenLimit)
	}
}

func TestAdmitBatchWithinEqualLargeBatchMatchesEqualSplit(t *testing.T) {
	store := NewResultStore(32 << 10)
	names := []string{"file_read", "file_read", "file_read", "file_read"}
	results := make([]Result, 4)
	for index := range results {
		results[index] = Result{Content: batchResultContent(8000)}
	}
	admitted := store.AdmitBatchWithin(names, results, 8000, 8000)
	var retained uint64
	for index, result := range admitted {
		if !result.Truncated {
			t.Fatalf("equally large result %d not truncated", index)
		}
		if result.Admission.TokenLimit != 2000 {
			t.Fatalf("waterline %d = %d, want 2000",
				index, result.Admission.TokenLimit)
		}
		retained += result.Admission.RetainedTokens
	}
	if retained > 8000 {
		t.Fatalf("retained aggregate %d exceeds pool", retained)
	}
}

func TestAdmitBatchWithinZeroPoolFallsBackToItemBudget(t *testing.T) {
	store := NewResultStore(32 << 10)
	names := []string{"file_read", "file_read"}
	results := []Result{
		{Content: batchResultContent(300)},
		{Content: batchResultContent(50)},
	}
	admitted := store.AdmitBatchWithin(names, results, 100, 0)
	if !admitted[0].Truncated || admitted[0].Admission.TokenLimit != 100 {
		t.Fatalf("large result = %+v", admitted[0].Admission)
	}
	if admitted[1].Truncated {
		t.Fatalf("small result truncated: %+v", admitted[1].Admission)
	}
}

func TestAdmitBatchWithinItemCapBoundsSingleResult(t *testing.T) {
	store := NewResultStore(32 << 10)
	admitted := store.AdmitBatchWithin(
		[]string{"file_read"},
		[]Result{{Content: batchResultContent(5000)}},
		1000, 8000,
	)
	if !admitted[0].Truncated || admitted[0].Admission.TokenLimit != 1000 {
		t.Fatalf("item cap not enforced: %+v", admitted[0].Admission)
	}
}

// A Chinese tool result must be admitted against its dense-script token
// count, not a quarter of it: otherwise CJK sessions inline four times the
// budgeted tokens before pressure pruning notices.
func TestResultAdmissionCountsDenseScriptTokens(t *testing.T) {
	store := NewResultStore(32 << 10)
	payload := strings.Repeat("配置文件内容", 100) // 600 runes, 1800 bytes
	admitted, receipt := store.Admit("file_read", Result{Content: payload})
	if admitted.Truncated {
		t.Fatalf("dense result was truncated: %+v", receipt)
	}
	if receipt.RetainedTokens != 600 {
		t.Fatalf("retained tokens = %d, want 600 dense-script tokens", receipt.RetainedTokens)
	}
	if receipt.TokenLimit < receipt.RetainedTokens {
		t.Fatalf("token limit %d below retained %d", receipt.TokenLimit, receipt.RetainedTokens)
	}
}

// A paged file_read continues at metadata.next_start_line instead of counting
// returned lines. The cursor has to survive both result admission (content
// truncation) and the model projection whitelist.
func TestModelResultPreservesReadPaginationCursor(t *testing.T) {
	input := Result{
		Content: "line-002\nline-003", Truncated: true,
		Metadata: map[string]any{
			"bytes": 4096, "start_line": 2, "returned_lines": 2,
			"max_lines": 2, "has_more": true, "next_start_line": 4,
		},
	}
	store := NewResultStore(16)
	admitted, receipt := store.Admit("file_read", input)
	if !admitted.Truncated || receipt.Handle == "" {
		t.Fatalf("admission = %+v receipt = %+v", admitted, receipt)
	}
	projected := ModelResult("file_read", admitted)
	metadata := projected.Metadata
	if metadata["has_more"] != true ||
		metadata["next_start_line"] != 4 ||
		metadata["returned_lines"] != 2 ||
		metadata["start_line"] != 2 {
		t.Fatalf("pagination cursor lost in projection: %#v", metadata)
	}
	data, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"next_start_line":4`) {
		t.Fatalf("model payload hides the cursor: %s", data)
	}
}
