package wire

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleContractsSatisfyDefaultOrder(t *testing.T) {
	if err := validateModuleContracts(defaultBuildModules()); err != nil {
		t.Fatalf("default module order violates its contract: %v", err)
	}
}

func TestModuleContractRejectsReadBeforeWriter(t *testing.T) {
	modules := []buildModule{
		buildModuleFunc{name: "reader", fn: func(
			context.Context, *buildState,
		) error {
			return nil
		}, contract: ModuleContract{Reads: []buildDomain{domainSecurity}}},
		buildModuleFunc{name: "writer", fn: func(
			context.Context, *buildState,
		) error {
			return nil
		}, contract: ModuleContract{Writes: []buildDomain{domainSecurity}}},
	}
	err := validateModuleContracts(modules)
	if err == nil ||
		!strings.Contains(err.Error(), "reads domain security") ||
		!strings.Contains(err.Error(), "reader") {
		t.Fatalf("read-before-write error = %v", err)
	}
}

func TestModuleContractRejectsDuplicateWriter(t *testing.T) {
	owner := ModuleContract{Writes: []buildDomain{domainTools}}
	modules := []buildModule{
		buildModuleFunc{name: "first", contract: owner, fn: func(
			context.Context, *buildState,
		) error {
			return nil
		}},
		buildModuleFunc{name: "second", contract: owner, fn: func(
			context.Context, *buildState,
		) error {
			return nil
		}},
	}
	err := validateModuleContracts(modules)
	if err == nil ||
		!strings.Contains(err.Error(), "already owned by first") ||
		!strings.Contains(err.Error(), "second") {
		t.Fatalf("duplicate writer error = %v", err)
	}
}

// TestModuleContractDeclarationsCoverBuildAccesses keeps each Contract()
// declaration in sync with the buildState domains its Build body touches
// directly. Accesses through same-package helpers are attributed by review:
// declaring a read makes the order validation enforce it, so a helper that
// starts touching a new domain must be reflected in the declaring module's
// contract. options and session are outside the domain model.
func TestModuleContractDeclarationsCoverBuildAccesses(t *testing.T) {
	byName := make(map[string]ModuleContract)
	for _, module := range defaultBuildModules() {
		byName[module.Name()] = module.Contract()
	}

	paths := append(mustGlob(t, "module*.go"), mustGlob(t, "modules_*.go")...)
	for _, path := range paths {
		fileset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileset, path, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != "Build" || function.Recv == nil {
				continue
			}
			module, ok := moduleByReceiver(receiverTypeName(function.Recv))
			if !ok {
				continue
			}
			contract := byName[module.Name()]
			declared := make(map[buildDomain]bool)
			for _, domain := range append(
				append([]buildDomain(nil), contract.Writes...), contract.Reads...,
			) {
				declared[domain] = true
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				base, ok := selector.X.(*ast.Ident)
				if !ok || base.Name != "state" {
					return true
				}
				domain := buildDomain(selector.Sel.Name)
				switch domain {
				case "options", "session":
					return true
				}
				if !declared[domain] {
					t.Errorf(
						"%s: module %s Build touches state.%s without declaring it",
						fileset.Position(selector.Pos()), module.Name(), domain,
					)
				}
				return true
			})
		}
	}
}

// TestBuildStateFieldsAreContractDomains keeps the buildState struct and the
// domain vocabulary in lockstep: every field is either an ordering-exempt
// root (options, session) or a declared domain, and every domain constant
// names a real field.
func TestBuildStateFieldsAreContractDomains(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "build_state.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]bool
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "buildState" {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("buildState is not a struct")
			}
			fields = make(map[string]bool)
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					fields[name.Name] = true
				}
			}
		}
	}
	if fields == nil {
		t.Fatal("buildState struct was not found")
	}
	for field := range fields {
		switch buildDomain(field) {
		case "options", "session":
		default:
			if !knownBuildDomains()[buildDomain(field)] {
				t.Errorf("buildState field %q has no contract domain", field)
			}
		}
	}
	for domain := range knownBuildDomains() {
		if !fields[string(domain)] {
			t.Errorf("contract domain %q names no buildState field", domain)
		}
	}
}

func knownBuildDomains() map[buildDomain]bool {
	domains := make(map[buildDomain]bool)
	for _, domain := range []buildDomain{
		domainConfig, domainProvider, domainPersistence, domainPlatform,
		domainTools, domainCapabilities, domainSecurity, domainOrchestration,
		domainAgent, domainRuntime,
	} {
		domains[domain] = true
	}
	return domains
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func receiverTypeName(field *ast.FieldList) string {
	if field == nil || len(field.List) == 0 {
		return ""
	}
	switch typeExpr := field.List[0].Type.(type) {
	case *ast.Ident:
		return typeExpr.Name
	case *ast.StarExpr:
		if ident, ok := typeExpr.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

func moduleByReceiver(typeName string) (buildModule, bool) {
	for _, module := range defaultBuildModules() {
		if receiverNames[typeName] == module.Name() {
			return module, true
		}
	}
	return nil, false
}

var receiverNames = map[string]string{
	"configModule":          "config",
	"providerModule":        "provider",
	"persistenceModule":     "persistence",
	"platformModule":        "platform",
	"builtinToolsModule":    "builtin-tools",
	"capabilityToolsModule": "capability-tools",
	"securityModule":        "security",
	"orchestrationModule":   "orchestration",
	"observabilityModule":   "observability",
	"agentModule":           "agent",
	"runtimeModule":         "runtime",
	"backgroundModule":      "background",
}
