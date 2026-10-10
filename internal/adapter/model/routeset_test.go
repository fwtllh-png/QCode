package model

import (
	"strings"
	"testing"
)

func TestASetWithoutSlotsAnswersEveryPurposeWithAct(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")

	routes, err := NewRouteSet(act, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, purpose := range []Purpose{PurposeAct, PurposeSummary, PurposeVision, PurposeJudge} {
		route, err := routes.For(purpose)
		if err != nil {
			t.Fatalf("For(%q) error = %v", purpose, err)
		}
		if route.Model().ID != "deepseek-v4-flash-vision-exp" {
			t.Fatalf("For(%q) model = %q, want the act model", purpose, route.Model().ID)
		}
	}
	if slots := routes.Slots(); slots != nil {
		t.Fatalf("Slots() = %v, want none", slots)
	}
}

func TestOneSlotChangesOnlyItsOwnPurpose(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	summary := testRoute(t, "openai", "gpt-4.1")

	routes, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeSummary: summary}, false)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := routes.For(PurposeSummary)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model().ID != "gpt-4.1" {
		t.Fatalf("summary model = %q, want gpt-4.1", resolved.Model().ID)
	}
	for _, purpose := range []Purpose{PurposeAct, PurposeVision} {
		route, err := routes.For(purpose)
		if err != nil {
			t.Fatalf("For(%q) error = %v", purpose, err)
		}
		if route.Model().ID != "deepseek-v4-flash-vision-exp" {
			t.Fatalf("For(%q) model = %q, want the act model", purpose, route.Model().ID)
		}
	}
	if slots := routes.Slots(); len(slots) != 1 || slots[0] != PurposeSummary {
		t.Fatalf("Slots() = %v, want [summary]", slots)
	}
}

func TestWithActPreservesPurposeSlotsAndLock(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	reasoner := testRoute(t, "deepseek", "deepseek-reasoner")
	routes, err := NewRouteSet(
		act,
		map[Purpose]ReadyRoute{PurposeSummary: reasoner},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := routes.WithAct(reasoner)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Act().Model().ID != "deepseek-reasoner" ||
		!updated.Locked() {
		t.Fatalf("updated route set = %+v", updated)
	}
	resolved, err := updated.For(PurposeSummary)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model().ID != "deepseek-reasoner" {
		t.Fatalf("summary route model = %q", resolved.Model().ID)
	}
}

func TestLockRefusesToFallBackInsteadOfSubstitutingAct(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	summary := testRoute(t, "openai", "gpt-4.1")

	routes, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeSummary: summary}, true)
	if err != nil {
		t.Fatal(err)
	}

	// The configured slot still resolves, and so does act itself: locking bans
	// the fallback, not the table.
	if resolved, err := routes.For(PurposeSummary); err != nil || resolved.Model().ID != "gpt-4.1" {
		t.Fatalf("For(summary) = %q, %v", resolved.Model().ID, err)
	}
	if acted, err := routes.For(PurposeAct); err != nil || acted.Model().ID != "deepseek-v4-flash-vision-exp" {
		t.Fatalf("For(act) = %q, %v", acted.Model().ID, err)
	}
	_, err = routes.For(PurposeVision)
	if err == nil || !strings.Contains(err.Error(), "route lock") {
		t.Fatalf("For(vision) error = %v, want a lock error", err)
	}
}

func TestSummaryAndJudgePurposesAreWired(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	summary := testRoute(t, "openai", "gpt-4.1")

	routes, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeSummary: summary}, false)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := routes.For(PurposeSummary)
	if err != nil || resolved.Model().ID != summary.Model().ID {
		t.Fatalf("For(summary) = %q, %v", resolved.Model().ID, err)
	}

	routes, err = NewRouteSet(act, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := routes.For(PurposeJudge); err != nil || resolved.Model().ID != act.Model().ID {
		t.Fatal("judge did not fall back to act")
	}
	routes, err = NewRouteSet(act, map[Purpose]ReadyRoute{PurposeJudge: summary}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := routes.For(PurposeJudge); err != nil || resolved.Model().ID != summary.Model().ID {
		t.Fatal("explicit judge was not selected")
	}
	routes, err = NewRouteSet(act, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routes.For(PurposeJudge); err == nil {
		t.Fatal("locked judge fell back")
	}
}

func TestActCannotBeConfiguredTwice(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	other := testRoute(t, "openai", "gpt-4.1")

	_, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeAct: other}, false)

	if err == nil || !strings.Contains(err.Error(), "execution.provider") {
		t.Fatalf("NewRouteSet() error = %v, want the act slot to be refused", err)
	}
}

func TestAnUnresolvedSetRefusesEveryPurpose(t *testing.T) {
	var routes RouteSet

	if routes.Ready() {
		t.Fatal("a zero RouteSet reports itself ready")
	}
	if _, err := routes.For(PurposeAct); err == nil {
		t.Fatal("a zero RouteSet resolved act; want a refusal")
	}
}

func TestSlotsAndPurposesKeepAStableOrder(t *testing.T) {
	act := testRoute(t, "deepseek-v4-flash", "deepseek-v4-flash-vision-exp")
	other := testRoute(t, "openai", "gpt-4.1")

	routes, err := NewRouteSet(act, map[Purpose]ReadyRoute{
		PurposeVision: other, PurposeSummary: other,
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	slots := routes.Slots()
	want := []Purpose{PurposeVision, PurposeSummary}
	if len(slots) != len(want) {
		t.Fatalf("Slots() = %v, want %v", slots, want)
	}
	for index, purpose := range want {
		if slots[index] != purpose {
			t.Fatalf("Slots() = %v, want %v", slots, want)
		}
	}
}

func TestAVisionSlotWithoutVisionIsRefusedAtConstruction(t *testing.T) {
	act := testRoute(t, "deepseek", "deepseek-chat")
	// deepseek-chat is an ordinary chat model: no vision bit in the catalog.
	blind := testRoute(t, "deepseek", "deepseek-chat")

	_, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeVision: blind}, false)
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("NewRouteSet() error = %v, want a vision capability refusal", err)
	}
}

func TestFallingBackToABlindActForVisionIsRefused(t *testing.T) {
	act := testRoute(t, "deepseek", "deepseek-chat")

	routes, err := NewRouteSet(act, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Summary is ordinary chat, so it still falls back.
	if _, routeErr := routes.For(PurposeSummary); routeErr != nil {
		t.Fatalf("For(%q) error = %v", PurposeSummary, routeErr)
	}
	_, err = routes.For(PurposeVision)
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("For(vision) error = %v, want a capability refusal on the act fallback", err)
	}
}
