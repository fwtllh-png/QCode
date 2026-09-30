package verify

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCommandReceiptKeepsDistinctCoverageForTheSameCommand(t *testing.T) {
	inputs := []Evidence{
		{Kind: "test", CommandDigest: "same-command", CoveredPaths: []string{"a.go"}, Status: StatusPassed, MutationRevision: 1, CallID: "a"},
		{Kind: "test", CommandDigest: "same-command", CoveredPaths: []string{"b.go"}, Status: StatusPassed, MutationRevision: 1, CallID: "b"},
	}
	receipt, missing := CommandEvidenceReceipt([]string{"a.go", "b.go"}, 1, inputs)
	if receipt.Status != StatusPassed || len(missing) != 0 || len(receipt.Checks) != 2 {
		t.Fatalf("separate coverage was overwritten: %+v", receipt)
	}
	inputs = append(inputs, Evidence{
		Kind: "test", CommandDigest: "same-command", CoveredPaths: []string{"a.go"},
		Status: StatusFailed, MutationRevision: 1, CallID: "a-again",
	})
	receipt, missing = CommandEvidenceReceipt([]string{"a.go", "b.go"}, 1, inputs)
	if receipt.Status != StatusFailed || !slices.Equal(missing, []string{"a.go"}) ||
		!slices.Equal(receipt.UncoveredPaths, missing) {
		t.Fatalf("failure did not replace matching coverage: %+v", receipt)
	}
}

func TestReceiptCoverageUsesFilteredAndRevalidatedEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, command string
		workspace     uint64
	}{
		{"configured command mismatch", "wrong-command", 4},
		{"stale workspace", "required-command", 3},
		{"changed input", "required-command", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &ReceiptRunner{Root: root, Command: "required-command"}
			receipt, err := runner.Verify(t.Context(), Request{
				Scope: ScopeCommands, Paths: []string{"a.go"}, MutationRevision: 1, WorkspaceRevision: 4,
				Evidence: []Evidence{{
					Kind: "test", Command: test.command, CommandDigest: "digest",
					InputDigest: "old-input", Status: StatusPassed, CoveredPaths: []string{"a.go"},
					MutationRevision: 1, WorkspaceRevision: test.workspace, CallID: "old-call",
				}},
			})
			if err != nil || receipt.Status != StatusUnavailable ||
				!slices.Equal(receipt.UncoveredPaths, []string{"a.go"}) {
				t.Fatalf("stale coverage survived: %+v %v", receipt, err)
			}
		})
	}
}

func TestVerificationDoesNotSilentlyDropInvalidPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "valid"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "outside"), "../outside", "", "."} {
		paths := []string{"valid", path}
		if _, err := InputDigest(root, paths); err == nil {
			t.Fatalf("invalid input %q was silently dropped", path)
		}
		runner := &ReceiptRunner{Root: root, Command: "true"}
		receipt, err := runner.Verify(t.Context(), Request{Scope: ScopeCommands, Paths: paths})
		if err != nil || receipt.Status != StatusUnavailable || len(receipt.UncoveredPaths) != 2 {
			t.Fatalf("invalid path counted as covered: %+v %v", receipt, err)
		}
	}
}

func TestDiagnosticsRequireEveryChangedPath(t *testing.T) {
	receipt := FromDiagnostics([]DiagnosticReceipt{
		{Path: "a.go", Status: "completed"},
		{Path: "b.go", Status: "unavailable"},
		{Path: "c.go", Status: "unknown"},
	}, []string{"a.go", "b.go", "c.go"})
	if receipt.Status != StatusUnavailable ||
		!slices.Equal(receipt.UncoveredPaths, []string{"b.go", "c.go"}) {
		t.Fatalf("partial diagnostics produced a pass: %+v", receipt)
	}
}

func TestConfiguredTemplateDoesNotExpandPlaceholdersInsidePaths(t *testing.T) {
	if got := expandCommand("check {paths}", []string{"{packages}.go"}); got != "check '{packages}.go'" {
		t.Fatalf("path content was interpreted as a template: %q", got)
	}
}
