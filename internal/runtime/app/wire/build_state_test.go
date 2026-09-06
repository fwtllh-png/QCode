package wire

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type buildModuleFunc struct {
	name     string
	contract ModuleContract
	fn       func(context.Context, *buildState) error
}

func (m buildModuleFunc) Name() string { return m.name }

func (m buildModuleFunc) Contract() ModuleContract { return m.contract }

func (m buildModuleFunc) Build(
	ctx context.Context,
	state *buildState,
) error {
	return m.fn(ctx, state)
}

func TestBuildModulesPreserveOrderAndStopAtFailure(t *testing.T) {
	failure := errors.New("stop")
	var order []string
	module := func(name string, err error) buildModule {
		return buildModuleFunc{name: name, fn: func(
			context.Context,
			*buildState,
		) error {
			order = append(order, name)
			return err
		}}
	}
	err := buildModules(
		t.Context(),
		&buildState{},
		module("first", nil),
		module("second", failure),
		module("unreachable", nil),
	)
	if !errors.Is(err, failure) {
		t.Fatalf("build error = %v", err)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("module order = %v, want %v", order, want)
	}
	if !strings.Contains(err.Error(), `module second`) {
		t.Fatalf("module identity missing from error: %v", err)
	}
}

func TestNewExecRollsBackResourcesWhenModuleFails(t *testing.T) {
	failure := errors.New("construction failed")
	var closed atomic.Int32
	modules := []buildModule{
		buildModuleFunc{name: "resource", fn: func(
			_ context.Context,
			state *buildState,
		) error {
			return state.session.resources.Add(
				"test-resource",
				func(context.Context) error {
					closed.Add(1)
					return nil
				},
			)
		}},
		buildModuleFunc{name: "failure", fn: func(
			context.Context,
			*buildState,
		) error {
			return failure
		}},
	}
	session, err := newExec(t.Context(), ExecOptions{}, modules)
	if session != nil || !errors.Is(err, failure) {
		t.Fatalf("NewExec = (%v, %v)", session, err)
	}
	if closed.Load() != 1 {
		t.Fatalf("rollback close count = %d, want 1", closed.Load())
	}
}

func TestNewExecRollsBackEveryConstructionBoundaryInReverseOrder(t *testing.T) {
	moduleNames := []string{
		"config", "provider", "persistence", "platform", "builtin-tools",
		"capability-tools", "security", "orchestration",
		"observability", "agent", "runtime", "background",
	}
	for failureIndex, failureName := range moduleNames {
		t.Run(failureName, func(t *testing.T) {
			failure := errors.New("injected construction failure")
			var built, closed []string
			modules := make([]buildModule, 0, len(moduleNames))
			for index, name := range moduleNames {
				modules = append(modules, buildModuleFunc{
					name: name,
					fn: func(
						_ context.Context,
						state *buildState,
					) error {
						built = append(built, name)
						if err := state.session.resources.Add(
							"synthetic-"+name,
							func(context.Context) error {
								closed = append(closed, name)
								return nil
							},
						); err != nil {
							return err
						}
						if index == failureIndex {
							return failure
						}
						return nil
					},
				})
			}
			session, err := newExec(t.Context(), ExecOptions{}, modules)
			if session != nil || !errors.Is(err, failure) {
				t.Fatalf("newExec = (%v, %v)", session, err)
			}
			wantBuilt := moduleNames[:failureIndex+1]
			if !reflect.DeepEqual(built, wantBuilt) {
				t.Fatalf("built modules = %v, want %v", built, wantBuilt)
			}
			wantClosed := append([]string(nil), wantBuilt...)
			for left, right := 0, len(wantClosed)-1; left < right; left, right = left+1, right-1 {
				wantClosed[left], wantClosed[right] = wantClosed[right], wantClosed[left]
			}
			if !reflect.DeepEqual(closed, wantClosed) {
				t.Fatalf("closed resources = %v, want %v", closed, wantClosed)
			}
		})
	}
}

func TestNewExecJoinsConstructionAndRollbackFailures(t *testing.T) {
	constructionErr := errors.New("construction failed")
	rollbackErr := errors.New("rollback failed")
	modules := []buildModule{
		buildModuleFunc{name: "resource", fn: func(
			_ context.Context,
			state *buildState,
		) error {
			return state.session.resources.Add(
				"failing-resource",
				func(context.Context) error { return rollbackErr },
			)
		}},
		buildModuleFunc{name: "failure", fn: func(
			context.Context,
			*buildState,
		) error {
			return constructionErr
		}},
	}
	session, err := newExec(t.Context(), ExecOptions{}, modules)
	if session != nil || !errors.Is(err, constructionErr) ||
		!errors.Is(err, rollbackErr) {
		t.Fatalf("newExec = (%v, %v)", session, err)
	}
	if !strings.Contains(err.Error(), `resource "failing-resource"`) {
		t.Fatalf("rollback resource identity missing from error: %v", err)
	}
}

func TestDefaultBuildModuleOrder(t *testing.T) {
	modules := defaultBuildModules()
	names := make([]string, 0, len(modules))
	for _, module := range modules {
		names = append(names, module.Name())
	}
	want := []string{
		"config", "provider", "persistence", "platform", "builtin-tools",
		"capability-tools", "security", "orchestration",
		"observability", "agent", "runtime", "background",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("module order = %v, want %v", names, want)
	}
}

func TestNewExecRemainsConstructionOnlyOrchestration(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("runtime.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "func NewExec(")
	if start < 0 {
		t.Fatal("NewExec source boundary was not found")
	}
	end := strings.Index(text[start:], "\nfunc configuredDiagnosticCommands")
	if end < 0 {
		t.Fatal("NewExec source boundary was not found")
	}
	newExec := text[start : start+end]
	for _, forbidden := range []string{
		"fixture.Start(",
		"builtin.NewWithIndex(",
		"toolguard.New(",
		"agentengine.New(",
		"NewPersistentRuntime(",
		"startScheduler(",
	} {
		if strings.Contains(newExec, forbidden) {
			t.Errorf("NewExec owns construction call %q", forbidden)
		}
	}
}

func TestRuntimeModuleDelegatesEngineConstructionToCoreBuilder(t *testing.T) {
	moduleSource, err := os.ReadFile("modules_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	coreSource, err := os.ReadFile("runtime_core.go")
	if err != nil {
		t.Fatal(err)
	}
	module := string(moduleSource)
	core := string(coreSource)
	for _, forbidden := range []string{
		"agentengine.New(",
		"bindEngineGuardFactory(",
		"childEngineOptions(",
		"childToolsets.open(",
	} {
		if strings.Contains(module, forbidden) {
			t.Errorf("agent module retained Runtime Core construction %q", forbidden)
		}
	}
	for _, required := range []string{
		"type runtimeCoreBuilder struct",
		"func (b runtimeCoreBuilder) BuildMain(",
		"func (b runtimeCoreBuilder) BuildChild(",
	} {
		if !strings.Contains(core, required) {
			t.Errorf("Runtime Core Builder is missing %q", required)
		}
	}
}

func TestWireDoesNotRegisterBackgroundOrchestrationTools(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("orchestration_components.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{
		`internal/adapter/tool/automation`,
		`internal/adapter/tool/task`,
		"automationtool.Register(",
		"tasktool.Register(",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("wire reintroduced orchestration tool owner %q", forbidden)
		}
	}
}

func TestModuleClosuresDoNotRetainBuildState(t *testing.T) {
	files, err := filepath.Glob("modules_*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		fileset := token.NewFileSet()
		file, parseErr := parser.ParseFile(
			fileset,
			path,
			nil,
			0,
		)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.FuncLit)
			if !ok {
				return true
			}
			ast.Inspect(literal.Body, func(inner ast.Node) bool {
				selector, selectorOK := inner.(*ast.SelectorExpr)
				if !selectorOK {
					return true
				}
				identifier, identifierOK := selector.X.(*ast.Ident)
				if identifierOK && identifier.Name == "state" {
					t.Errorf(
						"%s retains buildState in a closure at %s",
						path,
						fileset.Position(selector.Pos()),
					)
				}
				return true
			})
			return false
		})
	}
}

func TestBackgroundModuleOwnsRuntimeActivityStart(t *testing.T) {
	runtimeSource, err := os.ReadFile("modules_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	backgroundSource, err := os.ReadFile("module_background.go")
	if err != nil {
		t.Fatal(err)
	}
	runtimeBuild := string(runtimeSource)
	background := string(backgroundSource)
	if !strings.Contains(background, "func (backgroundModule) Build") {
		t.Fatal("BackgroundModule Build was not found")
	}
	for _, forbidden := range []string{".RefreshNow(", ".Start(ctx)", ".Tick("} {
		if strings.Contains(runtimeBuild, forbidden) {
			t.Errorf("RuntimeModule starts background activity %q", forbidden)
		}
	}
	required := []string{
		"prewarm.RefreshNow(ctx)",
		"state.runtime.application.Start(ctx)",
		"prewarm.Start(ctx)",
	}
	last := -1
	for _, fragment := range required {
		at := strings.Index(background, fragment)
		if at < 0 {
			t.Errorf("BackgroundModule is missing %q", fragment)
			continue
		}
		if at <= last {
			t.Errorf("BackgroundModule order is invalid at %q", fragment)
		}
		last = at
	}
	for _, path := range []string{"mcp.go", "modules_capabilities.go"} {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), "prewarm.Start(") {
			t.Errorf("%s starts MCP prewarm outside BackgroundModule", path)
		}
	}
}

func TestModulesFailClosedOnMissingRequirements(t *testing.T) {
	state := &buildState{}
	state.config.execution.Tools = true
	for _, test := range []struct {
		name   string
		module buildModule
	}{
		{name: "capability tools", module: capabilityToolsModule{}},
		{name: "child orchestration", module: orchestrationModule{}},
		{name: "prepared runtime", module: backgroundModule{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.module.Build(t.Context(), state); err == nil {
				t.Fatal("missing module requirement succeeded")
			}
		})
	}
}
