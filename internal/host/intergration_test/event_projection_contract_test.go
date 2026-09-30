package intergration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProjectUsesProtocolTraitsAndTerminalSemantics(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "op", ThreadID: "thread",
		TurnID: "turn", ItemID: "item",
	}, &protocol.TurnFailedData{Code: protocol.CodeInternal, Message: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	update, err := projectEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := update.(terminalUpdate)
	if !ok || terminal.traits().Class != protocol.EventClassTerminal ||
		terminal.Status != "failed" || !terminal.traits().Terminal {
		t.Fatalf("update = %+v", update)
	}
}

func TestProjectTreatsConvergenceAsStructuredIncomplete(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "op", ThreadID: "thread",
		TurnID: "turn", ItemID: "item",
	}, &protocol.TurnFailedData{
		Code: protocol.CodeConflict, Message: "turn needs continuation",
		Convergence: &protocol.TurnConvergence{
			Cause: "step_limit", Used: 4, Limit: 4,
			Summary:        "Progress retained.",
			PendingActions: []string{"Continue the remaining work."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := projectEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := update.(terminalUpdate)
	if !ok || terminal.Status != "incomplete" ||
		terminal.Convergence == nil ||
		terminal.Convergence.Cause != "step_limit" {
		t.Fatalf("update = %+v", update)
	}
}

func TestProjectPreservesSameVersionUnknownEventAsReadOnly(t *testing.T) {
	var event protocol.Event
	err := json.Unmarshal([]byte(`{
		"version":1,
		"id":"evt_future",
		"sequence":1,
		"operation_id":"op",
		"thread_id":"thread",
		"turn_id":"turn",
		"item_id":"item",
		"kind":"future.event",
		"created_at":"2026-08-18T00:00:00Z",
		"data":{"safe":true}
	}`), &event)
	if err != nil {
		t.Fatal(err)
	}
	update, err := projectEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	unknown, ok := update.(unknownUpdate)
	if !ok || unknown.Kind != "future.event" ||
		string(unknown.Raw) != `{"safe":true}` ||
		unknown.traits().Terminal {
		t.Fatalf("unknown update = %#v", update)
	}
}

func TestRecordedHostEventSequenceContract(t *testing.T) {
	type expectedProjection struct {
		AcceptedSequences []protocol.Cursor `json:"accepted_sequences"`
		Output            string            `json:"output"`
		Status            string            `json:"status"`
		UnknownEventKinds []string          `json:"unknown_event_kinds"`
		Terminal          bool              `json:"terminal"`
	}
	var fixture struct {
		Events           []json.RawMessage  `json:"events"`
		VersionSkewEvent json.RawMessage    `json:"version_skew_event"`
		Expected         expectedProjection `json:"expected"`
	}
	raw, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "testdata", "contracts", "host-event-sequence.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	var accepted []protocol.Cursor
	var output, status string
	var unknownKinds []string
	var terminal bool
	var cursor protocol.Cursor
	for _, rawEvent := range fixture.Events {
		var event protocol.Event
		if err := json.Unmarshal(rawEvent, &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence <= cursor {
			continue
		}
		cursor = event.Sequence
		accepted = append(accepted, event.Sequence)
		update, err := projectEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		switch projected := update.(type) {
		case textUpdate:
			if projected.Channel == "output" {
				output += projected.Text
			}
		case unknownUpdate:
			unknownKinds = append(unknownKinds, string(projected.Kind))
			if projected.traits().Terminal {
				t.Fatal("unknown event inferred terminal state")
			}
		case terminalUpdate:
			status = projected.Status
			terminal = projected.traits().Terminal
		}
	}
	if !reflect.DeepEqual(accepted, fixture.Expected.AcceptedSequences) ||
		output != fixture.Expected.Output ||
		status != fixture.Expected.Status ||
		!reflect.DeepEqual(unknownKinds, fixture.Expected.UnknownEventKinds) ||
		terminal != fixture.Expected.Terminal {
		t.Fatalf(
			"projection = sequences:%v output:%q status:%q unknown:%v terminal:%t; expected %+v",
			accepted, output, status, unknownKinds, terminal, fixture.Expected,
		)
	}

	var future protocol.Event
	if err := json.Unmarshal(fixture.VersionSkewEvent, &future); err == nil ||
		!strings.Contains(err.Error(), "unsupported event version 2") {
		t.Fatalf("version skew error = %v", err)
	}
}

func TestProjectProviderAttemptAsAccountingFact(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "op", ThreadID: "thread",
		TurnID: "turn", ItemID: "item",
	}, &protocol.ProviderAttemptData{
		SampleID: "sample-1", Attempt: 1, Status: protocol.ProviderAttemptRetryWait,
		FailureCode: "rate_limit", HTTPStatus: 429,
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := projectEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	accounting, ok := update.(accountingUpdate)
	if !ok || accounting.Attempt == nil ||
		accounting.Attempt.FailureCode != "rate_limit" ||
		accounting.traits().Class != protocol.EventClassAudit {
		t.Fatalf("update = %#v", update)
	}
}

func TestProjectAgentEventsWithoutHostPayloadSwitches(t *testing.T) {
	for _, data := range []protocol.EventData{
		&protocol.AgentSpawnedData{
			AgentID: "agent-1", WorkspaceRoot: "/workspace",
			SessionID: "session-1", Role: "explorer",
		},
		&protocol.AgentStatusData{
			AgentID: "agent-1", WorkspaceRoot: "/workspace",
			SessionID: "session-1", Status: "running",
		},
		&protocol.AgentMessageData{
			From: "agent-1", To: "parent", WorkspaceRoot: "/workspace",
			SessionID: "session-1", Sequence: 1,
			Body: []byte(`{"body":"done"}`),
		},
		&protocol.AgentIntegrationData{
			AgentID: "agent-1", AgentPath: "/root/explore",
			ParentPath: "/root", WorkspaceRoot: "/workspace",
			SessionID: "session-1", Status: "previewed",
			PreviewDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	} {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: 1, OperationID: "op", ThreadID: "thread",
			TurnID: "turn", ItemID: "item",
		}, data)
		if err != nil {
			t.Fatal(err)
		}
		update, err := projectEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		agent, ok := update.(agentUpdate)
		if !ok || agent.traits().ItemOwner != protocol.ItemOwner("agent") {
			t.Fatalf("update = %#v", update)
		}
	}
}
