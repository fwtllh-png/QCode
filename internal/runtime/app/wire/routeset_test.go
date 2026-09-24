package wire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func bundledAct() execRouteOptions {
	return execRouteOptions{
		ProviderID: "openai", ModelID: "gpt-4.1",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: fixtureModel("gpt-4.1"),
	}
}

func TestExplicitCredentialReferenceOverridesConnectionRoute(t *testing.T) {
	route, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai",
		ModelID:    "gpt-4.1",
		BaseURL:    "https://api.openai.com/v1",
		Protocol:   model.ProtocolOpenAIChat,
		Model:      fixtureModel("gpt-4.1"),
		Credential: model.CredentialRef{
			Kind: "env",
			Name: "WORKSPACE_OPENAI_KEY",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Credential() != (model.CredentialRef{
		Kind: "env",
		Name: "WORKSPACE_OPENAI_KEY",
	}) {
		t.Fatalf("credential=%+v", route.Credential())
	}
}

func TestASessionWithoutSlotsRoutesEveryPurposeToAct(t *testing.T) {
	routes, err := resolveRouteSet(routeSetOptions{Act: bundledAct()})
	if err != nil {
		t.Fatal(err)
	}

	for _, purpose := range []model.Purpose{
		model.PurposeAct, model.PurposePlan, model.PurposeVision,
		model.PurposeSummary,
	} {
		route, err := routes.For(purpose)
		if err != nil {
			t.Fatalf("For(%q) error = %v", purpose, err)
		}
		if route.Model().ID != "gpt-4.1" {
			t.Fatalf("For(%q) model = %q", purpose, route.Model().ID)
		}
	}
}

func TestASlotResolvesThroughTheConnectionsCatalog(t *testing.T) {
	plain := testCustomModel("gpt-4.1-mini")
	routes, err := resolveRouteSet(routeSetOptions{
		Act: bundledAct(),
		Additional: map[string]model.Model{
			plain.ID: plain,
		},
		Slots: map[string]config.RouteSlot{
			"plan": {Provider: "openai", Model: "gpt-4.1-mini"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := routes.For(model.PurposePlan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProviderID() != "openai" || plan.Model().ID != "gpt-4.1-mini" {
		t.Fatalf("plan route = %s/%s", plan.ProviderID(), plan.Model().ID)
	}
	if plan.Provenance() != model.ProvenanceConfig {
		t.Fatalf("plan provenance = %q, want config", plan.Provenance())
	}
}

func TestASlotRoutesToAnExtraConnection(t *testing.T) {
	secondary := customConnectionModel("secondary-model")
	routes, err := resolveRouteSet(routeSetOptions{
		Act: bundledAct(),
		Extras: []ExtraConnectionSpec{{
			ProviderID: "openai-compatible:def456",
			BaseURL:    "https://models.example.com/v1",
			Protocol:   model.ProtocolOpenAIChat,
			Model:      secondary,
		}},
		Slots: map[string]config.RouteSlot{
			"summary": {Provider: "openai-compatible:def456", Model: "secondary-model"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := routes.For(model.PurposeSummary)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderID() != "openai-compatible:def456" ||
		summary.Endpoint() != "https://models.example.com/v1" {
		t.Fatalf("summary route = %s %s", summary.ProviderID(), summary.Endpoint())
	}
}

func TestASlotNamingAnUnknownModelFailsTheSession(t *testing.T) {
	_, err := resolveRouteSet(routeSetOptions{
		Act: bundledAct(),
		Slots: map[string]config.RouteSlot{
			"vision": {Provider: "openai", Model: "gpt-9-imaginary"},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "route.vision") {
		t.Fatalf("resolveRouteSet() error = %v, want the slot named", err)
	}
}

func TestAFixtureSessionKeepsEverySlotOnTheFixture(t *testing.T) {
	act := execRouteOptions{
		ProviderID: "fixture", ModelID: "fixture-model", BaseURL: "http://127.0.0.1:1",
		Protocol: model.ProtocolOpenAIChat, Fixture: true, Model: fixtureModel("fixture-model"),
	}

	routes, err := resolveRouteSet(routeSetOptions{
		Act:   act,
		Slots: map[string]config.RouteSlot{"plan": {Provider: "fixture", Model: "planner"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := routes.For(model.PurposePlan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint() != act.BaseURL || plan.Model().ID != "planner" {
		t.Fatalf("plan route = %s %s", plan.Endpoint(), plan.Model().ID)
	}

	// A slot naming a catalog provider would leave the fixture and dial the real
	// thing, which would quietly falsify what a hermetic test claims.
	_, err = resolveRouteSet(routeSetOptions{
		Act:   act,
		Slots: map[string]config.RouteSlot{"plan": {Provider: "openai", Model: "gpt-4.1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "fixture provider") {
		t.Fatalf("resolveRouteSet() error = %v, want the fixture to be enforced", err)
	}
}

func TestASlotNamingAnUnconfiguredConnectionFailsTheSession(t *testing.T) {
	_, err := resolveRouteSet(routeSetOptions{
		Act: bundledAct(),
		Slots: map[string]config.RouteSlot{
			"plan": {Provider: "openai", Model: "other"},
		},
	})

	if err == nil ||
		!strings.Contains(err.Error(), "configured connection") {
		t.Fatalf("resolveRouteSet() error = %v, want the connection boundary explained", err)
	}
}

// TestAVisionSlotWithoutVisionFailsBeforeTheSessionStarts is the T3 acceptance:
// configuring [route.vision] with a text-only model is refused at resolve time,
// not later as a provider 400 about an image field.
func TestAVisionSlotWithoutVisionFailsBeforeTheSessionStarts(t *testing.T) {
	plain := testCustomModel("text-only-model")
	_, err := resolveRouteSet(routeSetOptions{
		Act: bundledAct(),
		Additional: map[string]model.Model{
			plain.ID: plain,
		},
		Slots: map[string]config.RouteSlot{
			"vision": {Provider: "openai", Model: "text-only-model"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("resolveRouteSet() error = %v, want a vision capability refusal", err)
	}
}

func TestLockedSlotsResolveAndLockedGapsDoNot(t *testing.T) {
	routes, err := resolveRouteSet(routeSetOptions{
		Act:   bundledAct(),
		Slots: map[string]config.RouteSlot{"plan": {Provider: "openai", Model: "gpt-4.1"}},
		Lock:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := routes.For(model.PurposePlan); err != nil {
		t.Fatalf("For(plan) error = %v", err)
	}
	if _, err := routes.For(model.PurposeVision); err == nil {
		t.Fatal("For(vision) fell back to act under a lock")
	}
}

// TestAPlanTurnInAFixtureSessionSamplesOnThePlanSlot is the end-to-end shape of
// this shard: a configuration file names a plan route, the session runs a turn in
// plan mode, and the turn reports the plan model rather than the act model.
func TestAPlanTurnInAFixtureSessionSamplesOnThePlanSlot(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "qcode.toml")
	if err := os.WriteFile(configPath, []byte(`
[route.plan]
provider = "fixture"
model = "planner"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	mode, tools := "plan", true
	session, err := NewExec(context.Background(), withNonDurableTestJournal(t, ExecOptions{
		ConfigPath:  configPath,
		FixturePath: subagentFixture(t, "openai"),
		Permission:  "bypass",
		ConfigOverrides: config.Overrides{
			Workspace: &workspace, Mode: &mode, Tools: &tools,
		},
	}))
	if err != nil {
		t.Fatalf("NewExec: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})

	events, err := session.Runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread", TurnID: "turn", ItemID: "prompt", Prompt: "say hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}

	var receipt *protocol.ExecutionReceiptData
	deadline := time.After(20 * time.Second)
	for receipt == nil {
		select {
		case event := <-events:
			if data, ok := event.Data.(*protocol.ExecutionReceiptData); ok {
				receipt = data
			}
		case <-deadline:
			t.Fatal("the turn produced no receipt")
		}
	}

	if len(receipt.Routes) != 1 {
		t.Fatalf("receipt routes = %+v, want one entry", receipt.Routes)
	}
	route := receipt.Routes[0]
	if route.Purpose != string(model.PurposePlan) || route.Model != "planner" {
		t.Fatalf("receipt route = %+v, want the plan slot", route)
	}
	if receipt.Mode != "plan" {
		t.Fatalf("receipt mode = %q", receipt.Mode)
	}
}
