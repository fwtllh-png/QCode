package config

import (
	"strings"
	"testing"
)

func TestASessionWithoutRouteSlotsHasAnEmptyTable(t *testing.T) {
	snapshot, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Config.Route.Lock {
		t.Fatal("route lock defaults to on")
	}
	if len(snapshot.Config.Route.Slots) != 0 {
		t.Fatalf("slots = %+v, want none", snapshot.Config.Route.Slots)
	}
}

func TestRouteSlotsAndLockComeOffTheFile(t *testing.T) {
	path := writeConfig(t, `
[route]
lock = true

[route.plan]
provider = "openai"
model = "gpt-4.1"
`)

	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if !snapshot.Config.Route.Lock {
		t.Fatal("route lock did not come off the file")
	}
	plan := snapshot.Config.Route.Slots["plan"]
	if plan.Provider != "openai" || plan.Model != "gpt-4.1" {
		t.Fatalf("plan slot = %+v", plan)
	}
	if snapshot.Provenance[fieldRouteProvider("plan")] != SourceFile ||
		snapshot.Provenance[fieldRouteLock] != SourceFile {
		t.Fatalf("provenance = %+v", snapshot.Provenance)
	}
}

func TestAMisspelledPurposeIsRefusedRatherThanIgnored(t *testing.T) {
	path := writeConfig(t, `
[route.planning]
provider = "openai"
model = "gpt-4.1"
`)

	_, err := Load(LoadOptions{Path: path})

	// The decoder refuses fields it does not know, which is what makes the closed
	// purpose set enforceable without a second check.
	if err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("Load() error = %v, want a refusal", err)
	}
}

func TestSummaryRouteComesOffTheFile(t *testing.T) {
	path := writeConfig(t, `
[route.summary]
provider = "openai"
model = "gpt-4.1"
`)

	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	summary := snapshot.Config.Route.Slots["summary"]
	if summary.Provider != "openai" || summary.Model != "gpt-4.1" {
		t.Fatalf("summary slot = %+v", summary)
	}
	if snapshot.Provenance[fieldRouteProvider("summary")] != SourceFile {
		t.Fatalf(
			"summary provider provenance = %q",
			snapshot.Provenance[fieldRouteProvider("summary")],
		)
	}
}

func TestAnUnwiredPurposeCannotBeConfigured(t *testing.T) {
	path := writeConfig(t, `
[route.judge]
provider = "openai"
model = "gpt-4.1"
`)

	_, err := Load(LoadOptions{Path: path})

	if err == nil || !strings.Contains(err.Error(), "strict mode") {
		t.Fatalf("Load() error = %v, want unwired purpose refusal", err)
	}
}

func TestAHalfNamedSlotIsAnError(t *testing.T) {
	path := writeConfig(t, `
[route.plan]
provider = "openai"
`)

	_, err := Load(LoadOptions{Path: path})

	if err == nil || !strings.Contains(err.Error(), "route.plan.model") {
		t.Fatalf("Load() error = %v, want the missing model named", err)
	}
}

func TestTheVisionSectionStillFillsTheVisionSlot(t *testing.T) {
	path := writeConfig(t, `
[vision]
enabled = true
provider = "openai"
model = "gpt-4.1"
`)

	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	slot := snapshot.Config.Route.Slots["vision"]
	if slot.Provider != "openai" || slot.Model != "gpt-4.1" {
		t.Fatalf("vision slot = %+v, want the [vision] section aliased into it", slot)
	}
	// The provenance points at the section the values came from, so `config show`
	// can still explain why the slot exists.
	if snapshot.Provenance[fieldRouteProvider("vision")] != SourceFile {
		t.Fatalf("provenance = %q", snapshot.Provenance[fieldRouteProvider("vision")])
	}
}

func TestAnExplicitVisionSlotWinsOverTheAlias(t *testing.T) {
	path := writeConfig(t, `
[vision]
enabled = true
provider = "openai"
model = "gpt-4.1"

[route.vision]
provider = "glm"
model = "glm-5.3"
`)

	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	slot := snapshot.Config.Route.Slots["vision"]
	if slot.Provider != "glm" || slot.Model != "glm-5.3" {
		t.Fatalf("vision slot = %+v, want the explicit slot", slot)
	}
}

func TestARouteSlotAloneEnablesNothingElse(t *testing.T) {
	// A [route.vision] slot without [vision] enabled is still a vision route: the
	// alias exists so old configurations keep working, not so that the new form
	// depends on the old one.
	path := writeConfig(t, `
[route.vision]
provider = "openai"
model = "gpt-4.1"
`)

	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Config.Vision.Enabled {
		t.Fatal("a route slot turned the legacy vision flag on")
	}
	if slot := snapshot.Config.Route.Slots["vision"]; slot.Provider != "openai" {
		t.Fatalf("vision slot = %+v", slot)
	}
}

func TestAnUntrustedRepositoryFileCannotRedirectARoute(t *testing.T) {
	repo := writeConfig(t, `
[route]
lock = true

[route.plan]
provider = "openai"
model = "gpt-4.1"
`)

	snapshot, err := Load(LoadOptions{RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}

	// A slot names an endpoint and a credential, so an untrusted project file that
	// could set one could redirect the session's traffic.
	if len(snapshot.Config.Route.Slots) != 0 || snapshot.Config.Route.Lock {
		t.Fatalf("untrusted route = %+v lock=%v", snapshot.Config.Route.Slots, snapshot.Config.Route.Lock)
	}

	trusted, err := Load(LoadOptions{RepoPath: repo, TrustRepo: true})
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Config.Route.Slots["plan"].Model != "gpt-4.1" {
		t.Fatalf("trusted route = %+v", trusted.Config.Route.Slots)
	}
}

func TestLockRouteOverrideBeatsTheFile(t *testing.T) {
	path := writeConfig(t, "[route]\nlock = false\n")
	locked := true

	snapshot, err := Load(LoadOptions{Path: path, Overrides: Overrides{RouteLock: &locked}})
	if err != nil {
		t.Fatal(err)
	}

	if !snapshot.Config.Route.Lock {
		t.Fatal("--lock-route did not win over the file")
	}
	if snapshot.Provenance[fieldRouteLock] != SourceStartup {
		t.Fatalf("provenance = %q", snapshot.Provenance[fieldRouteLock])
	}
}
