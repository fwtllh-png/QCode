package host

import "testing"

func TestUnaryRouteRegistryMatchesDispatcher(t *testing.T) {
	declared := make(map[string]RouteContract, len(unaryRouteContracts))
	for _, route := range unaryRouteContracts {
		if route.Path == "" || route.Method != "POST" ||
			route.Request == "" || route.Response == "" {
			t.Fatalf("incomplete route contract: %+v", route)
		}
		if _, duplicate := declared[route.Path]; duplicate {
			t.Fatalf("duplicate route contract %q", route.Path)
		}
		if route.Mutation != route.IdempotencyKey {
			t.Fatalf("route %q mutation/idempotency mismatch", route.Path)
		}
		declared[route.Path] = route
	}

	for path := range declared {
		if _, registered := unaryRouteHandler(path); !registered {
			t.Errorf("registered route %q has no generated handler", path)
		}
	}
	for path, request := range map[string]string{
		"agent/list":               "agent_query",
		"agent-preset/list":        "agent_preset_list",
		"agent-preset/save":        "agent_preset_save",
		"agent-preset/delete":      "agent_preset_delete",
		"agent-preset/apply":       "agent_preset_apply",
		"usage/query":              "usage_query",
		"extension/list":           "extension_query",
		"credential/status":        "empty",
		"credential/clear-keyring": "empty",
		"credential/validate":      "empty",
	} {
		if got := declared[path].Request; got != request {
			t.Errorf("route %q request = %q, want %q", path, got, request)
		}
	}
}

func TestHostContractReturnsDetachedSortedRoutes(t *testing.T) {
	first := Contract()
	if len(first.Routes) == 0 {
		t.Fatal("contract has no routes")
	}
	first.Routes[0].Path = "/mutated"
	second := Contract()
	if second.Routes[0].Path == "/mutated" {
		t.Fatal("Contract exposed mutable shared state")
	}
	for index := 1; index < len(second.Routes); index++ {
		if second.Routes[index-1].Path > second.Routes[index].Path {
			t.Fatalf("routes are not sorted at %d", index)
		}
	}
}
