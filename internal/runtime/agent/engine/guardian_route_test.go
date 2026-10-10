package engine

import (
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestGuardianRoutingFreezesConnectionAndFollowsCurrentAct(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	first, _ := e.PrepareGuardianReview()
	other := testRouteProtocol(t, "http://127.0.0.1:2", model.ProtocolOpenAIChat)
	scope := attachTestScope(t, e)
	scope.spec.Route = other
	e.syncSessionTitleState(provider.Usage{})
	second, err := e.PrepareGuardianReview()
	if err != nil || first.route.Endpoint() == second.route.Endpoint() || first.Versions().RouteDigest == second.Versions().RouteDigest {
		t.Fatal("act connection switch was not frozen", err)
	}
	judge := testRouteProtocol(t, "http://127.0.0.1:3", model.ProtocolOpenAIChat)
	e.options.Routes, _ = model.NewRouteSet(e.options.Route, map[model.Purpose]model.ReadyRoute{model.PurposeJudge: judge}, false)
	e.syncSessionTitleState(provider.Usage{})
	third, err := e.PrepareGuardianReview()
	if err != nil || third.route.Endpoint() != judge.Endpoint() {
		t.Fatal("explicit judge replaced by act", err)
	}
	if first.route.Endpoint() == other.Endpoint() {
		t.Fatal("in-flight review route mutated")
	}
}

func TestGuardianConfigurationAndPermissionGates(t *testing.T) {
	for _, name := range []string{"disabled", "kill_switch", "readonly", "full_access", "timeout", "negative_timeout", "output", "locked", "missing_gate"} {
		t.Run(name, func(t *testing.T) {
			e := guardianTestEngine(t, &guardianTestProvider{})
			switch name {
			case "disabled":
				e.options.Guardian.Enabled = false
			case "kill_switch":
				e.options.Security.DisableAutoReview = true
			case "readonly":
				e.options.Security.Permission = policy.PermissionNever
			case "full_access":
				e.options.Security.Permission = policy.PermissionBypass
			case "timeout":
				e.options.Guardian.Timeout = 0
			case "negative_timeout":
				e.options.Guardian.Timeout = -time.Second
			case "output":
				e.options.Guardian.MaxOutputTokens = e.options.Route.Model().Limits.MaxOutputTokens + 1
			case "locked":
				e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, true)
			case "missing_gate":
				e.options.SharedRateLimit = nil
			}
			e.syncSessionTitleState(provider.Usage{})
			if _, err := e.PrepareGuardianReview(); err == nil {
				t.Fatal("invalid configuration admitted")
			}
		})
	}
}

func TestGuardianReasoningDoesNotInheritActRequirement(t *testing.T) {
	act := testRoute(t)
	caps := act.Model().Capabilities
	caps.Reasoning = true
	caps.ReasoningEfforts = []string{"high"}
	caps.DefaultReasoningEffort = "high"
	act = act.WithCapabilities(caps)
	judge := testRoute(t)
	routes, err := model.NewRouteSet(act, map[model.Purpose]model.ReadyRoute{model.PurposeJudge: judge}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReasoningEffort(routes, "high"); err != nil {
		t.Fatal("act reasoning imposed on judge", err)
	}
}

func TestGuardianRouteOwnsFrozenMetadata(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	descriptor := e.options.Route.Model()
	descriptor.Capabilities.Reasoning = true
	descriptor.Capabilities.ReasoningEfforts = []string{"high"}
	price := 0.5
	descriptor.Pricing.CachedInputPerMillion = &price
	e.options.Route = e.options.Route.WithModel(descriptor)
	e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
	e.syncSessionTitleState(provider.Usage{})
	r, err := e.PrepareGuardianReview()
	if err != nil {
		t.Fatal(err)
	}
	price = 500
	e.options.Route.Model().Capabilities.ReasoningEfforts[0] = "changed"
	if *r.route.Model().Pricing.CachedInputPerMillion != 0.5 || r.route.Model().Capabilities.ReasoningEfforts[0] != "high" {
		t.Fatal("review model aliases mutable metadata")
	}
}

func TestGuardianExplicitOutputIsValidatedEvenWhenDisabled(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	if err := validateGuardianConfig(GuardianConfig{MaxOutputTokens: e.options.Route.Model().Limits.MaxOutputTokens + 1}, e.options.Routes); err == nil {
		t.Fatal("disabled flag hid incompatible explicit output")
	}
}
