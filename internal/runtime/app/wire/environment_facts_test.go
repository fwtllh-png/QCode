package wire

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
)

type preparationFactExecutor struct{ permissionsSyncExecutor }

func (preparationFactExecutor) Descriptor() tool.Descriptor {
	descriptor := permissionsSyncExecutor{}.Descriptor()
	descriptor.Name = "preparation_probe"
	return descriptor
}

func (preparationFactExecutor) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	return tool.Result{Metadata: map[string]any{"facts": environment.PreparationFactsFrom(ctx)}}, nil
}

func TestRuntimeGuardsUseTheirOwnEnvironmentPreparationFacts(t *testing.T) {
	workspace := t.TempDir()
	builder := childSkillBuilder(t, childSkillPaths(t, workspace), workspace, t.TempDir())
	if err := builder.seed.Tools.Register(preparationFactExecutor{}); err != nil {
		t.Fatal(err)
	}
	parentFacts := []environment.Fact{{Source: environment.SourcePreparer, Resource: "parent-only"}}
	builder.guardFactory = guardFactory{
		runtime: builder.seed.Security, registry: builder.seed.Tools,
		workspace: workspace, preparationFacts: parentFacts,
	}
	check := func(adapter *app.EngineAdapter, want []environment.Fact) {
		t.Helper()
		options := adapter.Underlying().OptionsSeed()
		guard, err := options.GuardFactory(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		result, err := guard.Execute(t.Context(), "facts", "preparation_probe", json.RawMessage(`{"url":"https://example.com/a"}`))
		if err != nil || result.IsError {
			t.Fatalf("execute facts: %v %+v", err, result)
		}
		got, _ := result.Metadata["facts"].([]environment.Fact)
		if !slices.Equal(got, want) {
			t.Fatalf("facts=%+v want=%+v", got, want)
		}
	}
	main, err := builder.BuildMain()
	if err != nil {
		t.Fatal(err)
	}
	check(main, parentFacts)
	for _, name := range []string{"child-auth", "sibling-auth"} {
		required := true
		builder.childTools.environment = config.Defaults().Execution.Environment
		builder.childTools.environment.Resources = []config.EnvironmentResource{{
			Name: name, Namespace: "credential", Access: "use", Host: "artifacts.example", Required: &required,
		}}
		root := t.TempDir()
		toolset, err := builder.childTools.open(root, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := toolset.registry.Register(preparationFactExecutor{}); err != nil {
			t.Fatal(err)
		}
		child, err := builder.BuildChild(app.ChildSpec{Workspace: root, HostWorkspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		check(child, []environment.Fact{{
			Source: environment.SourcePreparer, Category: environment.CategoryEnvironmentResourceUnavailable,
			Resource: name, RequiredAction: environment.ActionBindCredential, Detail: "credential_binder_unavailable",
		}})
	}
	check(main, parentFacts)
}
