package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestToolSearchUsesSessionProjectionFilter(t *testing.T) {
	for _, restriction := range []string{"profile", "blanket_deny", "resource_deny"} {
		t.Run(restriction, func(t *testing.T) {
			registry := tool.NewRegistry(nil, nil)
			if err := toolsearch.Register(registry); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"special_deploy", "weather_lookup"} {
				if err := registry.Register(catalogFixtureTool(name)); err != nil {
					t.Fatal(err)
				}
			}
			runtime := &scriptedProvider{streams: []provider.Stream{
				toolCallStream("search", toolsearch.ToolName, `{"query":"special_deploy","limit":1}`),
				textStream("done"), textStream("done"),
			}}
			engine := newEngine(t, runtime, registry)
			if restriction == "profile" {
				route := engine.options.Routes.Act()
				if err := engine.ApplySessionProfile(protocol.SessionProfile{
					Version: protocol.SessionProfileVersion, Revision: 1,
					Mode: "act", Provider: route.ProviderID(), Model: route.Model().ID,
					EnabledToolIDs:  []string{"builtin:tool_search", "builtin:result_get", "builtin:weather_lookup"},
					ApprovalPosture: "suggest", ExecutionTarget: "local",
					MaxSteps: 8, PromptCacheRevision: 1,
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				resource := "*"
				if restriction == "resource_deny" {
					resource = "protected"
				}
				if _, err := engine.options.Security.AppendManagedRule(policy.Rule{
					Tool: "special_deploy", Resource: resource, Action: policy.ActionDeny,
				}); err != nil {
					t.Fatal(err)
				}
			}
			var searchResult *tool.Result
			if _, err := engine.Execute(t.Context(), TurnRequest{Prompt: "find a capability"}, func(event Event) error {
				if event.ToolCall != nil && event.ToolCall.Name == toolsearch.ToolName && event.Result != nil {
					copy := *event.Result
					searchResult = &copy
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if searchResult == nil || searchResult.IsError || len(runtime.requests) < 2 {
				t.Fatalf("search=%+v samples=%d", searchResult, len(runtime.requests))
			}
			want := restriction == "resource_deny"
			if got := strings.Contains(searchResult.Content, `"name":"special_deploy"`); got != want {
				t.Fatalf("search leaked or hid tool: %s", searchResult.Content)
			}
			snapshot, err := registry.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			entry, _ := snapshot.Lookup("special_deploy")
			if got := entry.State == tool.CatalogEntryMaterialized; got != want {
				t.Fatalf("materialization disagrees with enabled filter: %+v", entry)
			}
			for _, request := range runtime.requests[1:] {
				found := false
				for _, definition := range request.Tools {
					found = found || definition.Name == "special_deploy"
				}
				if found != want {
					t.Fatalf("next sample disagrees with search: %+v", request.Tools)
				}
			}
		})
	}
}
