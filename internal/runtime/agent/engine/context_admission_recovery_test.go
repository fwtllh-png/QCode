package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"testing"
)

func TestContextReservationRejectionAllowsSmallerInputBatch(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	current := engine.ContextAdmission(nil, nil)
	engine.options.Context.TruthRetention.MandatoryMaxEntities = current.ProjectedEntities + 1
	calls := []provider.ToolCall{
		{ID: "input-a", Name: "request_user_input"},
		{ID: "input-b", Name: "request_user_input"},
	}
	rejected, err := engine.admitToolBatch(calls)
	if err != nil || rejected == nil || !rejected.IsError ||
		rejected.Metadata["error_category"] != "context_reservation_exceeded" ||
		rejected.Metadata["required_action"] != "split_batch_or_resolve_obligations" ||
		rejected.Metadata["executed"] != false {
		t.Fatalf("missing actionable rejection before execution: result=%+v err=%v", rejected, err)
	}
	if result, err := engine.admitToolBatch(calls[:1]); err != nil || result != nil {
		t.Fatalf("smaller batch still rejected: result=%+v err=%v", result, err)
	}
}

func TestWritesDoNotReserveMandatoryVerificationContext(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(t, 0, 4)
	engine := fixture.engine
	current := engine.ContextAdmission(nil, nil)
	engine.options.Context.TruthRetention.MandatoryMaxEntities = current.ProjectedEntities + 1
	calls := []provider.ToolCall{
		{ID: "write-a", Name: "file_write", Arguments: `{"path":"a.go","content":"a"}`},
		{ID: "write-b", Name: "file_write", Arguments: `{"path":"b.go","content":"b"}`},
	}
	snapshot, err := engine.options.Tools.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		binding, ok := snapshot.Binding(calls[i].Name)
		if !ok {
			t.Fatal("file_write binding is unavailable")
		}
		calls[i].CatalogID = binding.CatalogID
		calls[i].CatalogGeneration = binding.Generation
		calls[i].CatalogRevision = binding.Revision
		calls[i].CatalogAuthority = binding.Authority
		_, descriptor, _, err := engine.options.Tools.ResolveBound(calls[i].Name, binding)
		if err != nil || descriptor.AccessMode != tool.AccessWrite {
			t.Fatalf("fixture did not resolve a workspace write: %v", err)
		}
	}

	if result, err := engine.admitToolBatch(calls); err != nil || result != nil {
		t.Fatalf("writes reserved obsolete coverage obligations: result=%+v err=%v", result, err)
	}
}
