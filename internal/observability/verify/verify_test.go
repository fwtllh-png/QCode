package verify

import (
	"context"
	"strings"
	"testing"
)

func TestFromDiagnosticsGradesBySeverity(t *testing.T) {
	errorReceipt := DiagnosticReceipt{
		Path: "a.go", Status: "completed", Runner: "gopls",
		Diagnostics: []Diagnostic{{
			Path: "a.go", Severity: "error", Message: "undefined: foo",
			Range: DiagnosticRange{Start: DiagnosticPosition{Line: 4, Character: 2}},
		}},
	}
	warningReceipt := DiagnosticReceipt{
		Path: "b.go", Status: "completed", Runner: "gopls",
		Diagnostics: []Diagnostic{{
			Path: "b.go", Severity: "warning", Message: "shadowed variable",
		}},
	}

	tests := map[string]struct {
		receipts     []DiagnosticReceipt
		paths        []string
		wantStatus   string
		wantErrors   int
		wantWarnings int
	}{
		"error fails": {
			receipts: []DiagnosticReceipt{errorReceipt},
			paths:    []string{"a.go"}, wantStatus: StatusFailed, wantErrors: 1,
		},
		"warning passes": {
			receipts: []DiagnosticReceipt{warningReceipt},
			paths:    []string{"b.go"}, wantStatus: StatusPassed, wantWarnings: 1,
		},
		"runner failure fails": {
			receipts: []DiagnosticReceipt{{
				Path: "a.go", Status: "failed", Runner: "gopls", Message: "gopls exited with code 2",
			}},
			paths: []string{"a.go"}, wantStatus: StatusFailed, wantErrors: 1,
		},
		"unavailable is not a green light": {
			receipts: []DiagnosticReceipt{{Path: "a.go", Status: "unavailable"}},
			paths:    []string{"a.go"}, wantStatus: StatusUnavailable,
		},
		"no receipts at all": {paths: []string{"a.go"}, wantStatus: StatusUnavailable},
		"unrelated path is ignored": {
			receipts: []DiagnosticReceipt{errorReceipt},
			paths:    []string{"other.go"}, wantStatus: StatusUnavailable,
		},
		"absolute receipt path matches relative change": {
			receipts: []DiagnosticReceipt{{
				Path: "/workspace/pkg/a.go", Status: "completed", Runner: "gopls",
				Diagnostics: []Diagnostic{{Severity: "error", Message: "boom"}},
			}},
			paths: []string{"pkg/a.go"}, wantStatus: StatusFailed, wantErrors: 1,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := FromDiagnostics(test.receipts, test.paths)
			if receipt.Scope != ScopeDiagnostics || receipt.Status != test.wantStatus ||
				receipt.Errors != test.wantErrors || receipt.Warnings != test.wantWarnings {
				t.Fatalf("FromDiagnostics() = %+v, want status %q", receipt, test.wantStatus)
			}
			if receipt.Failed() != (test.wantStatus == StatusFailed) {
				t.Fatalf("Failed() = %v for status %q", receipt.Failed(), receipt.Status)
			}
		})
	}
}

func TestFromDiagnosticsFeedbackLocatesTheError(t *testing.T) {
	receipt := FromDiagnostics([]DiagnosticReceipt{{
		Path: "a.go", Status: "completed", Runner: "gopls",
		Diagnostics: []Diagnostic{{
			Path: "a.go", Severity: "error", Message: "undefined: foo",
			Range: DiagnosticRange{Start: DiagnosticPosition{Line: 4, Character: 2}},
		}},
	}}, nil)

	feedback := receipt.Feedback(0)
	if !strings.Contains(feedback, "a.go:5:3: undefined: foo") {
		t.Fatalf("Feedback() = %q", feedback)
	}
	if truncated := receipt.Feedback(20); !strings.HasSuffix(truncated, "truncated]") {
		t.Fatalf("Feedback(20) = %q, want truncation", truncated)
	}
}

func TestVerifyRejectsUnknownScope(t *testing.T) {
	runner := &ReceiptRunner{}
	if _, err := runner.Verify(
		context.Background(), Request{Scope: Scope("packages")},
	); err == nil {
		t.Fatal("Verify() accepted an unimplemented scope")
	}
}

func TestDiagnosticsScopeReadsTheRequestReceipts(t *testing.T) {
	runner := &ReceiptRunner{}
	receipt, err := runner.Verify(context.Background(), Request{
		Scope: ScopeDiagnostics, Paths: []string{"a.go"},
		Diagnostics: []DiagnosticReceipt{{
			Path: "a.go", Status: "completed", Runner: "gopls",
			Diagnostics: []Diagnostic{{Severity: "error", Message: "boom"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Failed() || receipt.Scope != ScopeDiagnostics {
		t.Fatalf("Verify() = %+v", receipt)
	}
}

func TestUnavailableRunnerNeverFails(t *testing.T) {
	receipt, err := UnavailableRunner{}.Verify(
		context.Background(), Request{Scope: ScopeRepository},
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != StatusUnavailable || receipt.Failed() {
		t.Fatalf("Verify() = %+v", receipt)
	}
}

func TestCommandEvidenceReceiptIgnoresRunningProcessCoverage(t *testing.T) {
	receipt, uncovered := CommandEvidenceReceipt(
		[]string{"a.go"},
		1,
		[]Evidence{{
			SchemaVersion: 1, Kind: "check", Status: StatusRunning,
			CoveredPaths: []string{"a.go"}, CommandDigest: "sha256:check",
			MutationRevision: 1,
		}},
	)
	if receipt.Status != StatusUnavailable || len(uncovered) != 1 ||
		uncovered[0] != "a.go" {
		t.Fatalf("receipt = %+v uncovered = %v", receipt, uncovered)
	}
}
