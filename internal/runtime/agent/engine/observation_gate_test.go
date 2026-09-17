package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestObservationGateRequiresWindowAfterSearchHit(t *testing.T) {
	engine := evidenceEngine(t)
	call := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"paxos_core.cpp"}`,
	}
	if blocked := engine.observationGate(call, false); blocked != nil {
		t.Fatalf("unlocated file_read blocked: %+v", blocked)
	}

	engine.observePath(agentcontext.SourceSearch, "paxos_core.cpp")
	if blocked := engine.observationGate(call, false); blocked != nil {
		t.Fatalf("path-only search hit blocked: %+v", blocked)
	}

	engine.observeEvidence(
		provider.ToolCall{Name: "search_text", Arguments: `{"query":"accept"}`},
		tool.Result{Outcome: &tool.Outcome{
			Facts: &tool.OutcomeFacts{Evidence: []tool.EvidenceHit{
				{Kind: tool.EvidenceReference, Path: "paxos_core.cpp", Line: 412},
			}},
		}},
	)
	if blocked := engine.observationGate(call, false); blocked != nil {
		t.Fatalf("located file_read without start_line = %+v", blocked)
	}

	windowed := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"paxos_core.cpp","start_line":412}`,
	}
	if got := engine.observationGate(windowed, false); got != nil {
		t.Fatalf("windowed file_read blocked: %+v", got)
	}
	other := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"types.h"}`,
	}
	if got := engine.observationGate(other, false); got != nil {
		t.Fatalf("unlocated sibling file_read blocked: %+v", got)
	}
	absoluteArgs, err := json.Marshal(map[string]string{
		"path": filepath.Join(engine.options.Workspace, "paxos_core.cpp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	absolute := provider.ToolCall{
		Name:      "file_read",
		Arguments: string(absoluteArgs),
	}
	if got := engine.observationGate(absolute, false); got != nil {
		t.Fatalf("absolute located file_read blocked: %+v", got)
	}
}

func TestObservationGateAdmitsUnwindowedReadInFinishOnly(t *testing.T) {
	engine := evidenceEngine(t)
	call := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"types.h"}`,
	}
	if blocked := engine.observationGate(call, true); blocked != nil {
		t.Fatalf("finish-only file_read without start_line = %+v", blocked)
	}
	windowed := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"types.h","start_line":88}`,
	}
	if got := engine.observationGate(windowed, true); got != nil {
		t.Fatalf("finish-only windowed file_read blocked: %+v", got)
	}
}

// A known-path record that cannot answer the read (no verifiable content
// version) must admit the necessary read instead of refusing it: refusing
// without serving the content is what forced the re-explore loop.
func TestObservationGateAdmitsNecessaryReadsOfKnownPaths(t *testing.T) {
	engine := evidenceEngine(t)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	if err := kernel.BindWorkItem(turnkernel.BindWorkItem{
		Goal: "你上一轮socket_transport_test测试时死锁了",
		KnownReads: map[string]turnkernel.WorkItemRead{
			"socket_transport.cpp": {Window: "full"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.admissionKernel = kernel
	before := engine.progressSignature(kernel)
	full := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"socket_transport.cpp"}`,
	}
	if got := engine.observationGate(full, false); got != nil {
		t.Fatalf("necessary full read of a seeded record blocked: %+v", got)
	}
	windowed := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"socket_transport.cpp","start_line":88}`,
	}
	if got := engine.observationGate(windowed, false); got != nil {
		t.Fatalf("windowed read of a seeded record blocked: %+v", got)
	}
	if after := engine.progressSignature(kernel); after != before {
		t.Fatalf("gate probes renewed signature: before=%q after=%q", before, after)
	}
}

func seedReadReplay(
	t *testing.T,
	engine *Engine,
	kernel *turnkernel.RuntimeKernel,
	path string,
	read turnkernel.WorkItemRead,
	entry readResultEntry,
) string {
	t.Helper()
	reads := map[string]turnkernel.WorkItemRead{path: read}
	if err := kernel.BindWorkItem(turnkernel.BindWorkItem{
		Goal:       "continue the investigation",
		KnownReads: reads,
	}); err != nil {
		t.Fatal(err)
	}
	engine.admissionKernel = kernel
	engine.readResultMu.Lock()
	engine.readResults[path] = entry
	engine.readResultMu.Unlock()
	return read.ContentDigest
}

func readReplayKernel(t *testing.T) *turnkernel.RuntimeKernel {
	t.Helper()
	return newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
}

// An unchanged file whose recorded window covers the request is answered
// with the original result, so the model keeps its evidence without paying
// for another exploration round.
func TestObservationGateReplaysCoveredUnchangedRead(t *testing.T) {
	engine := evidenceEngine(t)
	path := filepath.Join(engine.options.Workspace, "parser.go")
	if err := os.WriteFile(path, []byte("package parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, _, _, err := workspacejournal.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	kernel := readReplayKernel(t)
	seedReadReplay(t, engine, kernel, "parser.go",
		turnkernel.WorkItemRead{
			Window: "full", ContentDigest: fingerprint.SHA256,
		},
		readResultEntry{
			ContentDigest: fingerprint.SHA256,
			RequestIdentity: readRequestIdentity(provider.ToolCall{
				Name: "file_read", Arguments: `{"path":"parser.go"}`,
			}),
			CallID: "call-read-1",
			Turn:   1,
			Result: tool.Result{Content: "package parser\n"},
		},
	)
	full := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"parser.go"}`,
	}
	replay := engine.observationGate(full, false)
	if replay == nil || replay.IsError || replay.Content != "package parser\n" ||
		replay.Metadata["reused_read"] != true ||
		replay.Metadata["source_call_id"] != "call-read-1" {
		t.Fatalf("covered unchanged read replay = %+v", replay)
	}
	windowed := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"parser.go","start_line":9}`,
	}
	if got := engine.observationGate(windowed, false); got != nil {
		t.Fatalf("different window received the original body: %+v", got)
	}
}

func TestObservationGateAllowsReadWhenContentChanged(t *testing.T) {
	engine := evidenceEngine(t)
	path := filepath.Join(engine.options.Workspace, "parser.go")
	if err := os.WriteFile(path, []byte("package parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, _, _, err := workspacejournal.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	kernel := readReplayKernel(t)
	seedReadReplay(t, engine, kernel, "parser.go",
		turnkernel.WorkItemRead{
			Window: "full", ContentDigest: fingerprint.SHA256,
		},
		readResultEntry{
			ContentDigest: fingerprint.SHA256,
			RequestIdentity: readRequestIdentity(provider.ToolCall{
				Name: "file_read", Arguments: `{"path":"parser.go","start_line":1}`,
			}),
			CallID: "call-read-1",
			Turn:   1,
			Result: tool.Result{Content: "package parser\n"},
		},
	)
	if err := os.WriteFile(path, []byte("package parser // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := provider.ToolCall{
		ID:        "call-refresh",
		Name:      "file_read",
		Arguments: `{"path":"parser.go","start_line":1}`,
	}
	if got := engine.observationGate(changed, false); got != nil {
		t.Fatalf("stale record blocked a necessary read: %+v", got)
	}
	if reason := engine.takeReadInvalidation("call-refresh"); reason != "file_content_changed" {
		t.Fatalf("invalidation reason = %q", reason)
	}
}

func TestObservationGateAllowsReadWhenWindowUncovered(t *testing.T) {
	engine := evidenceEngine(t)
	path := filepath.Join(engine.options.Workspace, "parser.go")
	if err := os.WriteFile(path, []byte("package parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, _, _, err := workspacejournal.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	kernel := readReplayKernel(t)
	seedReadReplay(t, engine, kernel, "parser.go",
		turnkernel.WorkItemRead{
			Window:        "50",
			StartLine:     50,
			ContentDigest: fingerprint.SHA256,
		},
		readResultEntry{
			ContentDigest: fingerprint.SHA256,
			RequestIdentity: readRequestIdentity(provider.ToolCall{
				Name: "file_read", Arguments: `{"path":"parser.go","start_line":50}`,
			}),
			CallID: "call-read-1",
			Turn:   1,
			Result: tool.Result{Content: "window content"},
		},
	)
	earlier := provider.ToolCall{
		ID:        "call-earlier",
		Name:      "file_read",
		Arguments: `{"path":"parser.go","start_line":10}`,
	}
	if got := engine.observationGate(earlier, false); got != nil {
		t.Fatalf("uncovered window blocked a necessary read: %+v", got)
	}
	if reason := engine.takeReadInvalidation("call-earlier"); reason != "requested_window_not_covered" {
		t.Fatalf("invalidation reason = %q", reason)
	}
	covered := provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"parser.go","start_line":50}`,
	}
	if got := engine.observationGate(covered, false); got == nil || got.IsError {
		t.Fatalf("covered window read = %+v", got)
	}
}

func TestObservationGateAdmitsGitPatrolOnContinue(t *testing.T) {
	engine := evidenceEngine(t)
	kernel, err := turnkernel.NewRuntimeKernel(
		turnkernel.KernelIdentity{
			TurnID:          "continue-git",
			ProfileRevision: 1,
			Goal:            "你上一轮socket_transport_test测试时死锁了",
			WorkItem: turnkernel.WorkItem{
				KnownReads: map[string]turnkernel.WorkItemRead{
					"socket_transport.cpp": {Window: "full"},
				},
			},
		},
		protocol.TurnIntentAnswer,
		"act",
		&protocol.TurnRecoveryContext{
			Action:       protocol.TurnRecoveryContinue,
			SourceTurnID: "turn-source",
		},
		false,
		nil,
		nil,
		nil,
		nil,
		nil,
		turnkernel.DefaultPolicy(),
		turnkernel.NewEphemeralCoordinatorRuntime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	engine.admissionKernel = kernel
	before := engine.progressSignature(kernel)
	for _, name := range []string{"git_status", "git_diff"} {
		if blocked := engine.observationGate(provider.ToolCall{Name: name}, false); blocked != nil {
			t.Fatalf("%s patrol = %+v", name, blocked)
		}
	}
	if after := engine.progressSignature(kernel); after != before {
		t.Fatalf("git patrol refusal renewed signature: before=%q after=%q", before, after)
	}
}

func TestLocatedReadLineComesFromEvidenceLineHits(t *testing.T) {
	engine := evidenceEngine(t)
	engine.observeEvidence(
		provider.ToolCall{Name: "search_text", Arguments: `{"query":"accept"}`},
		tool.Result{Outcome: &tool.Outcome{
			Facts: &tool.OutcomeFacts{Evidence: []tool.EvidenceHit{
				{Kind: tool.EvidenceReference, Path: "paxos_core.cpp", Line: 412},
				{Kind: tool.EvidenceTextMatch, Path: "paxos_core.cpp", Line: 127},
			}},
		}},
	)
	sites := engine.locatedSites()
	if len(sites) != 2 || sites[0] != "paxos_core.cpp:412" ||
		sites[1] != "paxos_core.cpp:127" {
		t.Fatalf("located sites = %v", sites)
	}
	line, found := engine.locatedReadLine("paxos_core.cpp")
	if !found || line != 127 {
		t.Fatalf("located read line = %d, %t", line, found)
	}
	if blocked := engine.observationGate(provider.ToolCall{
		Name:      "file_read",
		Arguments: `{"path":"paxos_core.cpp"}`,
	}, false); blocked != nil {
		t.Fatalf("evidence-located file_read = %+v", blocked)
	}
}
