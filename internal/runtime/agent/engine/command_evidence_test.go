package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestAsyncCommandEvidenceBindsStartAndTerminalWithoutGrantingStaleCoverage(t *testing.T) {
	for _, test := range []struct {
		name                    string
		mutation                uint64
		terminated, changeInput bool
		want                    string
	}{
		{"natural exit", 1, false, false, verify.StatusPassed},
		{"new mutation", 2, false, false, verify.StatusUnavailable},
		{"external change", 1, false, true, verify.StatusUnavailable},
		{"terminated zero exit", 1, true, false, verify.StatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
			root := t.TempDir()
			engine.options.Workspace = root
			path := filepath.Join(root, "a.txt")
			if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			started := commandEvidenceResult(verify.StatusRunning, []string{"a.txt"})
			evidence := started.Outcome.Facts.Verification
			digest, err := verify.InputDigest(root, evidence.CoveredPaths)
			if err != nil {
				t.Fatal(err)
			}
			evidence.InputDigest, evidence.Command = digest, "test -f a.txt"
			started.Outcome.Facts.ProcessSession = &tool.ProcessSessionFact{
				SessionID: "term-1", SourceSessionID: "term-1", Running: true,
			}
			engine.bindVerificationEvidence(provider.ToolCall{ID: "start", Name: "exec_command"}, &started, false, 1)
			if len(engine.verificationEvidence()) != 0 {
				t.Fatal("running process counted as completed verification")
			}
			if test.changeInput {
				if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done := commandEvidenceResult(verify.StatusPassed, nil)
			done.Outcome.Facts.Verification = nil
			done.Outcome.Facts.ProcessSession = &tool.ProcessSessionFact{
				SourceSessionID: "term-1", ExitCode: 0, Terminated: test.terminated,
			}
			engine.bindVerificationEvidence(provider.ToolCall{ID: "poll", Name: "write_stdin"}, &done, false, test.mutation)
			got := engine.verificationEvidence()
			if len(got) != 1 || got[0].Status != test.want ||
				got[0].StartedCallID != "start" || got[0].CallID != "poll" ||
				got[0].InputDigest != digest || got[0].Command != "test -f a.txt" {
				t.Fatalf("bound evidence=%+v", got)
			}
			scope := engine.executionScope()
			if len(scope.state.pendingVerification) != 0 {
				t.Fatal("terminal evidence retained its pending session")
			}
			duplicate := commandEvidenceResult(verify.StatusPassed, nil)
			duplicate.Outcome.Facts.Verification = nil
			duplicate.Outcome.Facts.ProcessSession = done.Outcome.Facts.ProcessSession
			engine.bindVerificationEvidence(provider.ToolCall{ID: "poll-again", Name: "write_stdin"}, &duplicate, false, test.mutation)
			if len(engine.verificationEvidence()) != 1 {
				t.Fatal("terminal poll duplicated evidence")
			}
		})
	}
}

func TestBoundEvidenceMetadataMatchesFactsAndRejectsSameBatchProof(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	file := filepath.Join(engine.options.Workspace, "a.go")
	if err := os.WriteFile(file, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := commandEvidenceResult(verify.StatusPassed, []string{"a.go"})
	digest, err := verify.InputDigest(engine.options.Workspace, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	result.Outcome.Facts.Verification.InputDigest = digest
	result.Metadata = map[string]any{verify.EvidenceMetadataKey: *result.Outcome.Facts.Verification}
	if err := os.WriteFile(file, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine.bindVerificationEvidence(provider.ToolCall{ID: "finished", Name: "exec_command"}, &result, false, 1)
	metadata := result.Metadata[verify.EvidenceMetadataKey].(verify.Evidence)
	fact := result.Outcome.Facts.Verification
	if metadata.Status != verify.StatusUnavailable || metadata.Status != fact.Status ||
		metadata.CallID != "finished" || metadata.MutationRevision != fact.MutationRevision {
		t.Fatalf("metadata disagrees with bound facts: %+v %+v", metadata, fact)
	}
	result = commandEvidenceResult(verify.StatusPassed, []string{"a.go"})
	result.Metadata = map[string]any{verify.EvidenceMetadataKey: *result.Outcome.Facts.Verification}
	engine.bindVerificationEvidence(provider.ToolCall{ID: "parallel", Name: "exec_command"}, &result, true, 2)
	if result.Outcome.Facts.Verification != nil || result.Metadata[verify.EvidenceMetadataKey] != nil {
		t.Fatal("rejected same-batch proof remained in tool output")
	}
}

func TestVerificationRechecksInputsChangedOutsideFileTools(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	path := filepath.Join(engine.options.Workspace, "a.go")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	result := commandEvidenceResult(verify.StatusPassed, []string{"a.go"})
	digest, err := verify.InputDigest(engine.options.Workspace, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	result.Outcome.Facts.Verification.InputDigest = digest
	engine.bindVerificationEvidence(provider.ToolCall{ID: "verified", Name: "exec_command"}, &result, false, 1)
	if got := engine.verificationEvidence(); len(got) != 1 || got[0].Status != verify.StatusPassed {
		t.Fatalf("initial evidence = %+v", got)
	}
	// Full Access and host editors do not issue a file-tool mutation receipt.
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := engine.verificationEvidence(); len(got) != 1 || got[0].Status != verify.StatusInvalidated {
		t.Fatalf("stale evidence = %+v", got)
	}
}

func TestVerificationRepairIdentityDoesNotDependOnInvocationID(t *testing.T) {
	kernel := newEngineTurnKernel(protocol.TurnIntentWorkspaceChange, "act", nil, 0, nil, nil)
	seedKernelMutation(t, kernel)
	gate := verifyGate{kernel: kernel}
	receipt := verify.Receipt{Status: verify.StatusFailed, Checks: []verify.Check{{
		CallID: "first", Name: "test", Command: "test -f a.go", InputDigest: "same-input", Status: verify.StatusFailed,
	}}}
	first := gate.verificationCommand(receipt)
	receipt.Checks[0].CallID = "retry"
	second := gate.verificationCommand(receipt)
	if first.RepairKey != second.RepairKey || second.EvidenceCalls[0] != "retry" {
		t.Fatalf("invocation IDs reset repair budget: %+v %+v", first, second)
	}
}

type failedWriteWithChange struct {
	declarationWriteTool
	path string
}

func (f failedWriteWithChange) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	result, err := f.declarationWriteTool.Execute(ctx, raw)
	result.IsError = true
	result.Content = "command failed after a partial write"
	result.Outcome.Facts.WorkspaceChanges[0].Path = f.path
	return result, err
}

func TestFailedToolKeepsObservedChangeWithCanonicalPath(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(failedWriteWithChange{path: filepath.Join(root, "a.go")}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("failed-write", "write_fixture", `{}`),
		toolCallStream("complete", completiontool.Name,
			`{"status":"incomplete","summary":"partial write retained","pending_actions":["repair failed command"]}`),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())
	engine.options.Workspace = alias
	result, err := engine.Run(t.Context(), "record a partial write", nil)
	var problem *protocol.Problem
	if !errors.As(err, &problem) || problem.Code != protocol.CodeConflict ||
		result.State != Failed || len(result.Tools) != 2 {
		t.Fatalf("partial write did not reach incomplete finalization: result=%+v err=%v", result, err)
	}
	changes := engine.TurnDiff()
	if len(changes) != 1 || changes[0].Path != "a.go" || changes[0].Kind != tool.WorkspaceModified {
		t.Fatalf("failed tool change was hidden or used an alias: %+v", changes)
	}
}
