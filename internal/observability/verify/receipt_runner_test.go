package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiptRunnerNeverExecutesConfiguredCommand(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "not-executed")
	runner := &ReceiptRunner{Root: root, Command: "touch " + shellQuote(marker)}
	receipt, err := runner.Verify(t.Context(), Request{
		Scope: ScopeRepository, Paths: []string{"a.txt"}, MutationRevision: 1,
	})
	if err != nil || receipt.Status != StatusUnavailable ||
		!strings.Contains(receipt.Message, "exec_command") {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("verification gate executed a command")
	}
}

func TestHardPolicyCannotUseSelfSelectedCommandAsAcceptance(t *testing.T) {
	request := Request{Scope: ScopeAffected, Paths: []string{"a.go"}, MutationRevision: 1,
		RequireConfiguredCommand: true,
		Evidence: []Evidence{{Kind: "check", Status: StatusPassed, Command: "true", CommandDigest: "sha256:fixture",
			CoveredPaths: []string{"a.go"}, MutationRevision: 1}},
	}
	receipt, err := (&ReceiptRunner{}).Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusUnavailable || !strings.Contains(receipt.Message, "execution.verify.command") {
		t.Fatalf("hard policy accepted model-selected check: %+v %v", receipt, err)
	}
	request.RequireConfiguredCommand = false
	receipt, err = (&ReceiptRunner{}).Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusPassed {
		t.Fatalf("soft evidence was lost: %+v %v", receipt, err)
	}
}

func TestReceiptRunnerRequiresConfiguredCommandAndCurrentInput(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := InputDigest(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &ReceiptRunner{Root: root, Command: "test -f {paths}"}
	request := Request{
		Scope: ScopeAffected, Paths: []string{"a.txt"}, MutationRevision: 1,
		Evidence: []Evidence{{
			Kind: "check", Status: StatusPassed, Command: "true", CommandDigest: "sha256:fixture",
			InputDigest: digest, CoveredPaths: []string{"a.txt"}, MutationRevision: 1,
		}},
	}
	receipt, err := runner.Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusUnavailable {
		t.Fatalf("unrelated command met configured policy: %+v %v", receipt, err)
	}
	request.Evidence[0].Command = "test -f a.txt"
	request.Evidence[0].CWD = "other-project"
	receipt, err = runner.Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusUnavailable {
		t.Fatalf("command in another directory met configured policy: %+v %v", receipt, err)
	}
	request.Evidence[0].CWD = "."
	receipt, err = runner.Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusPassed || len(receipt.Checks) != 1 ||
		receipt.Checks[0].Command != "test -f a.txt" ||
		receipt.Checks[0].InputDigest != digest || receipt.Checks[0].MutationRevision != 1 {
		t.Fatalf("matching command did not pass: %+v %v", receipt, err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err = runner.Verify(t.Context(), request)
	if err != nil || receipt.Status != StatusUnavailable {
		t.Fatalf("stale inputs passed: %+v %v", receipt, err)
	}
}

func TestInputDigestTracksDeletedFilesAndRejectsEscapingSymlinks(t *testing.T) {
	root := t.TempDir()
	missing, err := InputDigest(root, []string{"removed.txt"})
	if err != nil || missing == "" {
		t.Fatalf("deleted file cannot be verified: %q %v", missing, err)
	}
	if err := os.WriteFile(filepath.Join(root, "removed.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty, err := InputDigest(root, []string{"removed.txt"})
	if err != nil || empty == missing {
		t.Fatal("deleted file and empty file have the same identity")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := InputDigest(root, []string{"escape"}); err == nil {
		t.Fatal("input snapshot read outside the workspace")
	}
}
