package shell

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func verificationRegistry(t *testing.T) (*tool.Registry, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, passthroughBackend{}); err != nil {
		t.Fatal(err)
	}
	return registry, root
}

func TestExecCommandProducesDeclaredEvidenceWithoutLanguageDefaults(t *testing.T) {
	registry, _ := verificationRegistry(t)
	for _, test := range []struct {
		command, status string
		exit            int
	}{
		{"test -f input.txt", verify.StatusPassed, 0},
		{"printf 'all tests passed'; exit 7", verify.StatusFailed, 7},
		{"false; printf masked", verify.StatusFailed, 1},
	} {
		result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
			"command": test.command, "verification": "check",
			"covered_paths": []string{"./input.txt", "input.txt"},
		})
		evidence := result.Outcome.Facts.Verification
		if evidence == nil || evidence.Status != test.status ||
			evidence.Command != test.command || evidence.ExitCode != test.exit ||
			evidence.InputDigest == "" || evidence.CommandDigest == "" ||
			len(evidence.CoveredPaths) != 1 || evidence.CallID != "" {
			t.Fatalf("evidence=%+v", evidence)
		}
	}
	plain := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{"command": "true"})
	if plain.Outcome.Facts.Verification != nil {
		t.Fatal("plain execution invented verification coverage")
	}
	inferred := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "test -f input.txt", "covered_paths": []string{"input.txt"},
	})
	evidence := inferred.Outcome.Facts.Verification
	if evidence == nil || evidence.Kind != "check" ||
		evidence.Status != verify.StatusPassed {
		t.Fatalf("covered_paths did not infer check evidence: %+v", evidence)
	}
}

func TestExecCommandRejectsInvalidVerificationBeforeExecution(t *testing.T) {
	registry, root := verificationRegistry(t)
	for _, fields := range []map[string]any{
		{"verification": "test"},
		{"verification": "made_up", "covered_paths": []string{"input.txt"}},
		{"verification": "test", "covered_paths": []string{"../outside"}},
		{"verification": "test", "covered_paths": []string{"/tmp/outside"}},
	} {
		fields["command"] = "touch should-not-run"
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tooltest.Execute(tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			ThreadID: processTestThread,
		}), registry, tool.Call{Name: "exec_command", Arguments: raw})
		if err == nil {
			t.Fatalf("invalid declaration accepted: %+v", fields)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatal("invalid declaration executed")
	}
}

func TestVerificationYieldDoesNotInventPassAndRetainsProcessIdentity(t *testing.T) {
	registry, _ := verificationRegistry(t)
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "sleep 0.1; exit 0", "yield_time_ms": 1,
		"verification": "test", "covered_paths": []string{"input.txt"},
	})
	facts := result.Outcome.Facts
	if facts.Verification == nil || facts.Verification.Status != verify.StatusRunning ||
		!facts.ProcessSession.Running || facts.ProcessSession.SourceSessionID == "" {
		t.Fatalf("running evidence=%+v", facts)
	}
	id := facts.ProcessSession.SourceSessionID
	done := executeProcessTool(t, registry, processTestThread, "write_stdin", map[string]any{
		"session_id": id, "yield_time_ms": 1000,
	})
	if done.Outcome.Facts.ProcessSession.SourceSessionID != id ||
		done.Outcome.Facts.ProcessSession.Running || done.Metadata["session_id"] != nil {
		t.Fatalf("terminal lost correlation or advertises stale session: %+v", done)
	}
}

func TestVerificationKeepsStartInputsAndRejectsTerminatedZeroExit(t *testing.T) {
	_, root := verificationRegistry(t)
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	protocol := &commandProtocol{workspace: workspace}
	input := execCommandInput{Command: "true", Verification: "test", CoveredPaths: []string{"input.txt"}}
	evidence, err := protocol.prepareVerification(input)
	if err != nil {
		t.Fatal(err)
	}
	result := tool.Result{Metadata: make(map[string]any)}
	attachVerification(&result, evidence, process.SessionWait{
		SessionRead: process.SessionRead{ExitCode: 0, Terminated: true},
	})
	if result.Metadata[verify.EvidenceMetadataKey].(verify.Evidence).Status != verify.StatusFailed {
		t.Fatal("terminated process counted as passing")
	}
	evidence, err = protocol.prepareVerification(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachVerification(&result, evidence, process.SessionWait{SessionRead: process.SessionRead{ExitCode: 0}})
	current, err := verify.InputDigest(root, evidence.CoveredPaths)
	if err != nil {
		t.Fatal(err)
	}
	recorded := result.Metadata[verify.EvidenceMetadataKey].(verify.Evidence)
	if recorded.InputDigest == current || recorded.InputDigest != evidence.InputDigest || recorded.CallID != "" {
		t.Fatal("adapter replaced the start snapshot instead of leaving binding to the engine")
	}
}

func TestVerificationWritingCoveredPathsIsInvalidatedAfterExecution(t *testing.T) {
	registry, root := verificationRegistry(t)
	// Build artifacts in an uncovered path stay valid evidence; rewriting a
	// covered input downgrades the evidence after execution instead of
	// rejecting the declaration up front.
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command":       "printf 'artifact' > artifact.txt",
		"verification":  "build",
		"covered_paths": []string{"input.txt"},
		"write_paths":   []string{"artifact.txt"},
	})
	evidence := result.Outcome.Facts.Verification
	if evidence == nil || evidence.Status != verify.StatusPassed {
		t.Fatalf("uncovered artifact write invalidated evidence: %+v", evidence)
	}
	body, err := os.ReadFile(filepath.Join(root, "artifact.txt"))
	if err != nil || string(body) != "artifact" {
		t.Fatalf("artifact = %q err=%v", body, err)
	}

	result = executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command":       "printf 'rewritten' > input.txt",
		"verification":  "build",
		"covered_paths": []string{"input.txt"},
		"write_paths":   []string{"input.txt"},
	})
	evidence = result.Outcome.Facts.Verification
	if evidence == nil || evidence.Status != verify.StatusInvalidated {
		t.Fatalf("covered-path write was not invalidated: %+v", evidence)
	}
	if evidence.InvalidationReason == "" {
		t.Fatalf("invalidation reason is empty: %+v", evidence)
	}
	if body, err := os.ReadFile(filepath.Join(root, "input.txt")); err != nil || string(body) != "rewritten" {
		t.Fatalf("input.txt = %q err=%v", body, err)
	}
}
