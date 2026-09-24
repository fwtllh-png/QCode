package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestEngineRejectsUnadvertisedFixedReasoningEffort(t *testing.T) {
	options := Options{ProviderConfig: ProviderConfig{Provider: &scriptedProvider{},
		Route: testRoute(t),

		ReasoningEffort: "unsupported"}, ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)},
	}
	if _, err := newTestEngine(options); err == nil ||
		!strings.Contains(err.Error(), "does not support reasoning effort") {
		t.Fatalf("New() error = %v", err)
	}
	resolver, err := model.NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	options.Route, err = resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek-v4-flash",
		ModelID:    "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	options.ReasoningEffort = "off"
	engine, err := newTestEngine(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.options.ReasoningEffort; got != "off" {
		t.Fatalf("reasoning effort = %q", got)
	}
}
