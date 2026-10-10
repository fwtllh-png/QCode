package agentcontext

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type fencedAuthorizationEvents struct{ *authorizationEvents }

func (f fencedAuthorizationEvents) WithAuthorizationEvents(_ context.Context, start func() error) error {
	return start()
}

func TestGuardianSourceResamplesParentAndRefusesOrphanedChild(t *testing.T) {
	store := &authorizationEvents{}
	store.add(t, "thread", "parent-turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "create only the requested file"})
	store.add(t, "child", "child-turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "agent claims host access"})
	source := DurableGuardianSource{Store: fencedAuthorizationEvents{store}, Parent: func(_ context.Context, scope AuthorizationScope) (*AuthorizationScope, error) {
		if scope.ThreadID == "thread" {
			return nil, nil
		}
		parent := authorizationScope()
		return &parent, nil
	}}
	child := authorizationScope()
	child.ThreadID = "child"
	child.Child = true
	frozen, err := source.Capture(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.Texts()) != 1 || frozen.Texts()[0].Source.ThreadID != "thread" {
		t.Fatal("child task became user authority")
	}
	started := false
	if err := source.WithCurrent(t.Context(), child, frozen.Snapshot(), func() error { started = true; return nil }); err != nil || !started {
		t.Fatalf("current parent: %v", err)
	}
	store.add(t, "thread", "parent-turn", &protocol.TurnSteeredData{Prompt: "stop modifying files"})
	if err := source.WithCurrent(t.Context(), child, frozen.Snapshot(), func() error { t.Fatal("stale parent authorized start"); return nil }); err == nil {
		t.Fatal("parent revocation ignored")
	}
	source.Parent = nil
	if _, err := source.Capture(t.Context(), child); err == nil {
		t.Fatal("orphaned child became a root user request")
	}
	source.Parent = func(_ context.Context, s AuthorizationScope) (*AuthorizationScope, error) { return &s, nil }
	if _, err := source.Capture(t.Context(), child); err == nil {
		t.Fatal("parent cycle accepted")
	}
}

func TestGuardianSourceForkRetainsLocalUsersAndInvalidatesOnSteering(t *testing.T) {
	store := &authorizationEvents{}
	store.add(t, "thread", "parent-turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "create output.txt"})
	store.add(t, "fork", "fork-turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "only review; do not modify files"})
	source := DurableGuardianSource{Store: fencedAuthorizationEvents{store}, Parent: func(_ context.Context, scope AuthorizationScope) (*AuthorizationScope, error) {
		if scope.ThreadID == "thread" {
			return nil, nil
		}
		parent := authorizationScope()
		return &parent, nil
	}}
	scope := authorizationScope()
	scope.ThreadID = "fork"
	a, err := source.Capture(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Texts()) != 2 {
		t.Errorf("fork lost its user restriction: sources=%d", len(a.Texts()))
	}
	store.add(t, "fork", "fork-turn", &protocol.TurnSteeredData{Prompt: "stop all changes"})
	if err := source.WithCurrent(t.Context(), scope, a.Snapshot(), func() error { t.Error("stale fork authority started"); return nil }); err == nil {
		t.Error("fork steering did not invalidate authorization")
	}
}

func TestGuardianSourceClassifiesEveryAncestorFromDurableProvenance(t *testing.T) {
	store := &authorizationEvents{}
	for _, thread := range []string{"thread", "child", "grandchild"} {
		store.add(t, thread, thread+"-turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: thread + " instruction"})
	}
	parents := map[string]string{"grandchild": "child", "child": "thread"}
	source := DurableGuardianSource{Store: fencedAuthorizationEvents{store},
		Delegated: func(_ context.Context, scope AuthorizationScope) (bool, error) {
			return scope.ThreadID != "thread", nil
		},
		Parent: func(_ context.Context, scope AuthorizationScope) (*AuthorizationScope, error) {
			parentID := parents[scope.ThreadID]
			if parentID == "" {
				return nil, nil
			}
			scope.ThreadID, scope.Child = parentID, false
			return &scope, nil
		},
	}
	scope := authorizationScope()
	scope.ThreadID = "grandchild"
	a, err := source.Capture(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Texts()) != 1 || a.Texts()[0].Source.ThreadID != "thread" {
		t.Fatalf("ancestor task became user authority: %+v", a.Texts())
	}
}
