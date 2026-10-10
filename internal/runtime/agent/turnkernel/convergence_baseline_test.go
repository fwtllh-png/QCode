package turnkernel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const convergenceExitGateEnv = "QCODE_TURN_KERNEL_CONVERGENCE_EXIT_GATE"

type convergenceDeviation struct {
	id             string
	classification string
	detail         string
	resolved       bool
	active         func(*testing.T) bool
}

func TestC0FoundationOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	owners := productionReducerApplyOwners(t, root)
	want := []string{"internal/runtime/agent/turnkernel/coordinator.go"}
	if !slices.Equal(owners, want) {
		t.Fatalf("Reducer.Apply production owners = %v, want %v", owners, want)
	}

	file := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/coordinator.go",
	)
	for _, name := range []string{
		"NewTurnCoordinator",
		"RestoreTurnCoordinator",
		"Submit",
		"transition",
	} {
		if findFunction(file, name) == nil {
			t.Fatalf("TurnCoordinator foundation function %q is missing", name)
		}
	}
	if sites := productionIdentifierSites(
		t,
		root,
		"RuntimeCaptureReplay",
	); len(sites) != 0 {
		t.Fatalf("Runtime Capture Replay production symbols remain: %v", sites)
	}
	replayPackage := filepath.Join(
		root,
		"internal/runtime/agent/turnkernel/replay",
	)
	if _, err := os.Stat(replayPackage); !os.IsNotExist(err) {
		t.Fatalf("independent Turn Kernel replay package remains: %v", err)
	}
}

func TestC2ControlToolOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	engine := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/runtime_kernel.go",
	)
	if !structHasFieldType(
		engine,
		"RuntimeKernel",
		"DurableEffectDispatcher",
	) {
		t.Fatal("RuntimeKernel does not use DurableEffectDispatcher")
	}
	for _, name := range []string{
		"observeToolLocked",
		"resolveWaitLocked",
		"resolveWaitAuthoritativeLocked",
		"closeToolObservedLocked",
	} {
		if findFunction(engine, name) != nil {
			t.Fatalf("C2 bridge function %q remains", name)
		}
	}
	for _, name := range []string{"Claim", "Complete", "Forget"} {
		if fileCalls(engine, name) {
			t.Fatalf("Engine still calls manual dispatcher method %q", name)
		}
	}
	dispatcher := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/effect_dispatcher.go",
	)
	if findFunction(dispatcher, "C2RoutedEffect") == nil {
		t.Fatal("C2 routed Effect ownership function is missing")
	}
	for _, name := range []string{"RequireApproval", "RequireInput"} {
		if !functionCalls(findFunction(engine, name), "Start") {
			t.Fatalf("%s does not persist EffectStarted before waiting", name)
		}
	}
}

func TestC3ModelDecisionOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	turnHandler := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/turn_handler.go",
	)
	run := findFunction(turnHandler, "Execute")
	if functionCalls(run, "requestRepair") {
		t.Fatal("Engine turn handler still spends repair budgets")
	}
	if functionHasIdentifier(run, "completionVerification") {
		t.Fatal("Engine turn handler still owns completion verification state")
	}
	completionFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/completion_declaration.go",
	)
	for _, name := range []string{
		"bindCompletionDeclaration",
		"completionDecisionFromResult",
	} {
		if findFunction(completionFile, name) != nil {
			t.Fatalf("Engine completion decision function %q remains", name)
		}
	}
	commandFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/command.go",
	)
	for _, field := range []string{
		"Mode",
		"RequirePassed",
		"OnFailure",
		"RepairLimit",
	} {
		if structHasNamedField(commandFile, "VerificationFinished", field) {
			t.Fatalf("Verification Result Command still carries policy field %q", field)
		}
	}
	kernelFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/runtime_kernel.go",
	)
	if findFunction(kernelFile, "observeVerificationLocked") != nil {
		t.Fatal("Verification Engine event still reverse-drives Kernel")
	}
	dispatcher := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/effect_dispatcher.go",
	)
	if findFunction(dispatcher, "C3RoutedEffect") == nil {
		t.Fatal("C3 routed Effect ownership function is missing")
	}
}

func TestC3ConvergenceBudgetOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	for _, identifier := range []string{
		"maxOutputContinuations",
		"OutputContinuations",
		"noProgressProblem",
		"maxCompletionRepairs",
		"maxWorkspaceChangeRepairs",
		"maxDeclarationRepairs",
		"OutputLimit",
		"maxModelVisibleItemTokens",
		"visionMaxOutputTokens",
	} {
		if sites := productionIdentifierSites(t, root, identifier); len(sites) != 0 {
			t.Fatalf(
				"local convergence terminal %s remains in production: %v",
				identifier,
				sites,
			)
		}
	}
	handler := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/turn_handler.go",
	)
	run := findFunction(handler, "Execute")
	if functionCalls(run, "RequestConvergence") {
		t.Fatal("Engine turn loop still owns work-step convergence")
	}
	if fileHasIdentifier(handler, "CodeResourceExhausted") {
		t.Fatal("Engine turn loop still chooses a resource-exhausted terminal")
	}
	progress := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/progress.go",
	)
	if fileHasIdentifier(progress, "CodeResourceExhausted") {
		t.Fatal("Engine progress policy still chooses a resource-exhausted terminal")
	}
	state := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/state.go",
	)
	if !structHasNamedFieldType(
		state,
		"State",
		"Convergence",
		"ConvergenceState",
	) {
		t.Fatal("Turn Kernel state does not own convergence")
	}
	command := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/command.go",
	)
	for _, name := range []string{
		"ConvergenceRequested",
		"ConvergenceFinalizationStarted",
	} {
		if !hasTypeDeclaration(command, name) {
			t.Fatalf("Turn Kernel convergence command %q is missing", name)
		}
	}
}

func TestC4TerminalCommitOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	kernelFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/runtime_kernel.go",
	)
	if findFunction(kernelFile, "observeTerminalLocked") != nil {
		t.Fatal("Terminal Engine event still reverse-drives Kernel")
	}
	eventsFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/events.go",
	)
	if fileHasType(eventsFile, "TerminalObservations") {
		t.Fatal("TerminalObservations event assembly remains")
	}
	applicationFile := parseProductionFile(
		t,
		root,
		"internal/runtime/app/engine_adapter.go",
	)
	commit := findFunction(applicationFile, "commitTerminal")
	if functionCalls(commit, "Emit") {
		t.Fatal("EngineAdapter terminal commit retains split Emit fallback")
	}
	storeFile := parseProductionFile(
		t,
		root,
		"internal/persist/state/turnstate/store.go",
	)
	if findFunction(storeFile, "CommitTerminalOperation") == nil {
		t.Fatal("atomic Terminal/Operation SQLite commit port is missing")
	}
}

func TestFaultDispositionOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	protocolFile := parseProductionFile(
		t,
		root,
		"internal/runtime/protocol/fault.go",
	)
	for _, name := range []string{
		"FaultMetadata",
		"FaultDisposition",
		"SideEffectState",
	} {
		if !hasTypeDeclaration(protocolFile, name) {
			t.Fatalf("protocol Fault contract %q is missing", name)
		}
	}
	terminalFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/terminal_handler.go",
	)
	if !functionCalls(findFunction(terminalFile, "setPrimary"), "ProblemOf") {
		t.Fatal("Turn terminal boundary does not classify errors through ProblemOf")
	}
	operationFile := parseProductionFile(
		t,
		root,
		"internal/runtime/app/operation_failure.go",
	)
	if !functionCalls(findFunction(operationFile, "operationRejection"), "ProblemOf") {
		t.Fatal("Runtime operation boundary bypasses Fault classification")
	}
	stateFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/turnkernel/state.go",
	)
	if !structHasNamedField(stateFile, "TerminalDecision", "Fault") {
		t.Fatal("durable TerminalDecision does not retain Fault metadata")
	}
}

func TestC5RestartProjectionOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	publisherFile := parseProductionFile(
		t,
		root,
		"internal/runtime/app/terminal_publisher.go",
	)
	if findFunction(publisherFile, "Recover") == nil {
		t.Fatal("Terminal Publisher has no terminal outbox recovery")
	}
	startFile := parseProductionFile(
		t,
		root,
		"internal/runtime/app/runtime_start.go",
	)
	if !functionCalls(findFunction(startFile, "activate"), "Recover") {
		t.Fatal("Runtime startup does not invoke Terminal Publisher recovery")
	}
	storeFile := parseProductionFile(
		t,
		root,
		"internal/persist/state/turnstate/store.go",
	)
	if findFunction(storeFile, "PendingTerminalProjections") == nil {
		t.Fatal("SQLite Store has no global pending terminal projection scan")
	}
	protocolFile := parseProductionFile(
		t,
		root,
		"internal/runtime/protocol/event.go",
	)
	if findFunction(protocolFile, "NewEventWithIdentity") == nil {
		t.Fatal("protocol has no stable Event identity constructor")
	}
}

func TestC6SingleAuthorityOwnershipBaseline(t *testing.T) {
	root := convergenceRepositoryRoot(t)
	for _, identifier := range []string{
		"DeferredEffectDispatcher",
		"MigrationEffectDispatcher",
		"applyLocked",
		"drifted",
		"terminalProjector",
		"normalizeTerminalProjection",
		"EffectCommitTerminal",
		"EffectPublishOutbox",
		"BeginTerminal",
	} {
		if sites := productionIdentifierSites(
			t,
			root,
			identifier,
		); len(sites) != 0 {
			t.Fatalf(
				"legacy identifier %s remains in production: %v",
				identifier,
				sites,
			)
		}
	}
	handlerFile := parseProductionFile(
		t,
		root,
		"internal/runtime/agent/engine/turn_handler.go",
	)
	run := findFunction(
		handlerFile,
		"Execute",
	)
	if functionCalls(run, "RequireApproval") ||
		functionCalls(run, "RequireInput") {
		t.Fatal("Engine Event projection still reverse-drives Kernel commands")
	}
	applicationFile := parseProductionFile(
		t,
		root,
		"internal/runtime/app/engine_adapter.go",
	)
	if functionCalls(findFunction(applicationFile, "commitTerminal"), "Emit") {
		t.Fatal("non-transactional terminal emit fallback remains")
	}
}

func TestC0OwnershipFailureBaseline(t *testing.T) {
	for _, deviation := range c0ConvergenceDeviations() {
		t.Run(deviation.id, func(t *testing.T) {
			active := deviation.active(t)
			if active == deviation.resolved {
				t.Fatalf(
					"C0-C1 baseline %s active=%t, want %t: %s",
					deviation.classification,
					active,
					!deviation.resolved,
					deviation.detail,
				)
			}
		})
	}
}

func TestC0OwnershipExitGate(t *testing.T) {
	if os.Getenv(convergenceExitGateEnv) != "1" {
		t.Skip("set " + convergenceExitGateEnv + "=1 to enforce the convergence exit gate")
	}
	for _, deviation := range c0ConvergenceDeviations() {
		t.Run(deviation.id, func(t *testing.T) {
			if deviation.active(t) {
				t.Fatalf(
					"%s deviation remains: %s",
					deviation.classification,
					deviation.detail,
				)
			}
		})
	}
}

func c0ConvergenceDeviations() []convergenceDeviation {
	return []convergenceDeviation{
		{
			id:             "C0-D01-engine-coordinator-memory-store",
			classification: "Missing",
			detail:         "Engine constructs the production TurnCoordinator with MemoryTerminalEnvelopeStore",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				engineKernel := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/turnkernel/runtime_kernel.go",
				)
				engine := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/engine/engine.go",
				)
				engineOptions := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/engine/options_types.go",
				)
				wire := parseProductionFile(
					t,
					root,
					"internal/runtime/app/wire/modules_runtime.go",
				)
				contextPolicy := parseProductionFile(
					t,
					root,
					"internal/runtime/app/wire/context_policy.go",
				)
				function := findFunction(
					engineKernel,
					"NewRuntimeKernel",
				)
				return functionCalls(
					function,
					"NewMemoryTerminalEnvelopeStore",
				) ||
					fileCalls(engine, "NewEphemeralCoordinatorRuntime") ||
					!structHasNamedFieldType(
						engineOptions,
						"LifecycleConfig",
						"TurnCoordinatorRuntime",
						"CoordinatorRuntime",
					) ||
					!(fileCalls(wire, "NewCoordinatorRuntime") ||
						fileCalls(
							contextPolicy,
							"NewCoordinatorRuntime",
						))
			},
		},
		{
			id:             "C0-D02-restore-has-no-production-caller",
			classification: "Missing",
			detail:         "RestoreTurnCoordinator has no production call site",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				return len(productionCallSites(
					t,
					root,
					"RestoreTurnCoordinator",
				)) == 0
			},
		},
		{
			id:             "C0-D03-deferred-dispatcher-preserves-engine-executor",
			classification: "Bridge",
			detail:         "Control and Tool Effects use DeferredEffectDispatcher or manual Claim/Complete/Forget",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				file := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/turnkernel/runtime_kernel.go",
				)
				return structHasFieldType(
					file,
					"RuntimeKernel",
					"DeferredEffectDispatcher",
				) ||
					fileCalls(file, "Claim") &&
						fileCalls(file, "Complete") &&
						fileCalls(file, "Forget")
			},
		},
		{
			id:             "C0-D04-turn-executor-retains-business-decisions",
			classification: "Legacy",
			detail:         "Execute still selects Repair and keeps completion-verification state",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				file := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/engine/turn_handler.go",
				)
				function := findFunction(
					file,
					"Execute",
				)
				return functionCalls(function, "requestRepair") ||
					functionHasIdentifier(function, "completionVerification")
			},
		},
		{
			id:             "C0-D05-engine-events-reverse-drive-kernel",
			classification: "Bridge",
			detail:         "Verification Engine events still reverse-drive Kernel commands",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				kernelFile := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/turnkernel/runtime_kernel.go",
				)
				return findFunction(
					kernelFile,
					"observeVerificationLocked",
				) != nil
			},
		},
		{
			id:             "C0-D05-terminal-events-reverse-drive-kernel",
			classification: "Foundation",
			detail:         "Terminal Engine events no longer reverse-drive Kernel commands",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				kernelFile := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/turnkernel/runtime_kernel.go",
				)
				return findFunction(
					kernelFile,
					"observeTerminalLocked",
				) != nil
			},
		},
		{
			id:             "C0-D08-parallel-terminal-fallbacks",
			classification: "Foundation",
			detail:         "Runtime no longer synthesizes a parallel terminal outcome",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				engineFile := parseProductionFile(
					t,
					root,
					"internal/runtime/agent/engine/terminal_handler.go",
				)
				finish := findFunction(engineFile, "finish")
				runtimeFile := parseProductionFile(
					t,
					root,
					"internal/runtime/app/runtime.go",
				)
				start := findFunction(runtimeFile, "start")
				return functionCalls(finish, "send") &&
					functionHasCompositeType(start, "TurnFailedData") &&
					fileHasType(
						parseProductionFile(
							t,
							root,
							"internal/runtime/agent/engine/events.go",
						),
						"State",
					)
			},
		},
		{
			id:             "C0-D09-app-nontransactional-terminal-fallback",
			classification: "Foundation",
			detail:         "EngineAdapter requires one TerminalCommitSink and has no split emit fallback",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				file := parseProductionFile(
					t,
					root,
					"internal/runtime/app/engine_adapter.go",
				)
				function := findFunction(file, "commitTerminal")
				return functionCalls(function, "CommitTerminal") &&
					functionCalls(function, "Emit")
			},
		},
		{
			id:             "C0-D10-pending-outbox-has-no-production-recovery",
			classification: "Foundation",
			detail:         "Runtime startup resumes pending Outbox projection with stable Event identities",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				return len(productionCallSites(
					t,
					root,
					"PendingOutbox",
				)) == 0
			},
		},
		{
			id:             "C0-D11-frozen-engine-terminal-assembly",
			classification: "Foundation",
			detail:         "Coordinator freezes terminal state without TerminalObservations event assembly",
			resolved:       true,
			active: func(t *testing.T) bool {
				root := convergenceRepositoryRoot(t)
				return len(productionIdentifierSites(
					t,
					root,
					"FrozenTurnKernel",
				)) != 0 ||
					len(productionIdentifierSites(
						t,
						root,
						"TerminalObservations",
					)) != 0
			},
		},
	}
}

func convergenceRepositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(workingDirectory, "../../../.."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	return root
}

func parseProductionFile(
	t *testing.T,
	root string,
	path string,
) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(
		token.NewFileSet(),
		filepath.Join(root, filepath.FromSlash(path)),
		nil,
		0,
	)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

func findFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	return nil
}

func hasTypeDeclaration(file *ast.File, name string) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if ok && typeSpec.Name.Name == name {
				return true
			}
		}
	}
	return false
}

func functionCalls(function *ast.FuncDecl, name string) bool {
	if function == nil || function.Body == nil {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && calledName(call.Fun) == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func fileCalls(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && calledName(call.Fun) == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func fileHasIdentifier(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func calledName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func structHasFieldType(
	file *ast.File,
	structName string,
	typeName string,
) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != structName {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			for _, field := range structType.Fields.List {
				if expressionTypeName(field.Type) == typeName {
					return true
				}
			}
		}
	}
	return false
}

func structHasNamedFieldType(
	file *ast.File,
	structName string,
	fieldName string,
	typeName string,
) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != structName {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			for _, field := range structType.Fields.List {
				if expressionTypeName(field.Type) != typeName {
					continue
				}
				for _, name := range field.Names {
					if name.Name == fieldName {
						return true
					}
				}
			}
		}
	}
	return false
}

func structHasNamedField(
	file *ast.File,
	structName string,
	fieldName string,
) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != structName {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					if name.Name == fieldName {
						return true
					}
				}
			}
		}
	}
	return false
}

func fileHasType(file *ast.File, typeName string) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if ok && typeSpec.Name.Name == typeName {
				return true
			}
		}
	}
	return false
}

func expressionTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	case *ast.StarExpr:
		return expressionTypeName(value.X)
	default:
		return ""
	}
}

func functionHasCompositeType(
	function *ast.FuncDecl,
	typeName string,
) bool {
	if function == nil || function.Body == nil {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if ok && expressionTypeName(literal.Type) == typeName {
			found = true
			return false
		}
		return !found
	})
	return found
}

func functionHasIdentifier(
	function *ast.FuncDecl,
	name string,
) bool {
	if function == nil || function.Body == nil {
		return false
	}
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func productionCallSites(
	t *testing.T,
	root string,
	name string,
) []string {
	t.Helper()
	var sites []string
	fileSet := token.NewFileSet()
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || calledName(call.Fun) != name {
				return true
			}
			relative, err := filepath.Rel(root, path)
			if err == nil {
				sites = append(sites, filepath.ToSlash(relative))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(sites)
	return sites
}

func productionIdentifierSites(
	t *testing.T,
	root string,
	name string,
) []string {
	t.Helper()
	var sites []string
	fileSet := token.NewFileSet()
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok && identifier.Name == name {
				found = true
			}
			return true
		})
		if found {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sites = append(sites, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(sites)
	return sites
}

func productionReducerApplyOwners(
	t *testing.T,
	root string,
) []string {
	t.Helper()
	var owners []string
	fileSet := token.NewFileSet()
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(
		path string,
		entry os.DirEntry,
		err error,
	) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Apply" {
				return true
			}
			receiver, ok := selector.X.(*ast.SelectorExpr)
			if ok && receiver.Sel.Name == "reducer" {
				found = true
			}
			return true
		})
		if found {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			owners = append(owners, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(owners)
	return owners
}
