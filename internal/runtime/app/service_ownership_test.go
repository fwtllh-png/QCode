package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeOwnedState lists each Runtime service's private state. Only methods
// on the owning type may read or write these fields; other services go
// through the owner's methods. TurnService.active is deliberately absent: the
// ActiveTurnRegistry synchronizes itself and is shared through its methods.
var runtimeOwnedState = map[string][]string{
	"SessionService": {"mutationMu", "titleWorkers", "titleMu", "titleJobs"},
	"EventService": {
		"publishMu", "mu", "terminals", "approvals", "inputs", "observerMu",
		"observers", "nextObserver", "observerQueue", "observerDispatching",
		"toolItems", "approvalItems", "inputItems",
	},
	"OperationService": {
		"mu", "operations", "processed", "accepted", "acceptedKeys",
		"committed", "accepting", "workspaceOperation", "withdrawing", "changed",
		"settlements",
	},
	"TurnQueueService": {"mu", "items", "claims"},
	"TurnService":      {"workers", "deferred"},
}

// runtimeStateConstructors build services and may initialize their state.
var runtimeStateConstructors = map[string]bool{
	"installRuntimeServices": true,
	"newTurnQueueService":    true,
}

// TestRuntimeServicesOwnTheirState fails when code outside an owning service
// touches that service's private state, whether through the service value
// (r.EventService.approvals), a local alias, or field promotion through
// Runtime or a handler that embeds it (r.approvals).
func TestRuntimeServicesOwnTheirState(t *testing.T) {
	files := parseRuntimePackage(t)
	structs := runtimeStructFields(files)
	for owner, fields := range runtimeOwnedState {
		for _, field := range fields {
			if !structs[owner].fields[field] {
				t.Fatalf("%s has no field %s; update runtimeOwnedState", owner, field)
			}
		}
	}
	checked := 0
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || runtimeStateConstructors[function.Name.Name] {
				continue
			}
			resolver := newOwnershipResolver(function, structs)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.AssignStmt:
					resolver.bind(value)
				case *ast.SelectorExpr:
					owner := resolver.stateOwner(value)
					if owner == "" {
						return true
					}
					checked++
					if owner != resolver.receiver {
						t.Errorf(
							"%s: %s accesses %s.%s; use a %s method",
							parsed.fileset.Position(value.Sel.Pos()),
							function.Name.Name, owner, value.Sel.Name, owner,
						)
					}
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("ownership scan resolved no service state access")
	}
}

// TestRuntimeServicesDoNotEmbedRuntime keeps the facade one-directional:
// Runtime embeds its services, but a service embedding *Runtime back would
// silently promote every other service's state into its method set.
func TestRuntimeServicesDoNotEmbedRuntime(t *testing.T) {
	structs := runtimeStructFields(parseRuntimePackage(t))
	services := []string{
		"SessionService", "AgentPresetService", "EventService",
		"RecoveryService", "OperationService", "TurnService", "TurnQueueService",
	}
	for _, service := range services {
		entry, ok := structs[service]
		if !ok {
			t.Fatalf("service %s not found", service)
		}
		for _, embedded := range entry.embeds {
			if embedded == "Runtime" {
				t.Errorf("%s embeds *Runtime; hold it in a named runtime field", service)
			}
		}
		if entry.named["runtime"] != "Runtime" {
			t.Errorf("%s has no named runtime *Runtime field", service)
		}
	}
}

type parsedRuntimeFile struct {
	fileset *token.FileSet
	file    *ast.File
}

func parseRuntimePackage(t *testing.T) []parsedRuntimeFile {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []parsedRuntimeFile
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileset := token.NewFileSet()
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, parsedRuntimeFile{fileset: fileset, file: file})
	}
	return files
}

type runtimeStruct struct {
	fields map[string]bool
	// embeds names package-local struct types embedded by pointer or value.
	embeds []string
	// named maps field names to their package-local struct type.
	named map[string]string
}

func runtimeStructFields(files []parsedRuntimeFile) map[string]runtimeStruct {
	structs := map[string]runtimeStruct{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			body, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			entry := runtimeStruct{fields: map[string]bool{}, named: map[string]string{}}
			for _, field := range body.Fields.List {
				typeName := localTypeName(field.Type)
				if len(field.Names) == 0 {
					if typeName != "" {
						entry.embeds = append(entry.embeds, typeName)
					}
					continue
				}
				for _, name := range field.Names {
					entry.fields[name.Name] = true
					if typeName != "" {
						entry.named[name.Name] = typeName
					}
				}
			}
			structs[spec.Name.Name] = entry
			return true
		})
	}
	return structs
}

func localTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

type ownershipResolver struct {
	receiver string
	structs  map[string]runtimeStruct
	idents   map[string]string
}

func newOwnershipResolver(function *ast.FuncDecl, structs map[string]runtimeStruct) *ownershipResolver {
	resolver := &ownershipResolver{
		receiver: receiverService(function),
		structs:  structs,
		idents:   map[string]string{},
	}
	if name := receiverName(function); name != "" {
		resolver.idents[name] = resolver.receiver
	}
	for _, field := range function.Type.Params.List {
		typeName := localTypeName(field.Type)
		if _, known := structs[typeName]; !known {
			continue
		}
		for _, name := range field.Names {
			resolver.idents[name.Name] = typeName
		}
	}
	return resolver
}

// bind records local variables whose package-local struct type is known, such
// as r := s.runtime or s := r.OperationService.
func (r *ownershipResolver) bind(assign *ast.AssignStmt) {
	if len(assign.Lhs) != len(assign.Rhs) {
		return
	}
	for index, left := range assign.Lhs {
		ident, ok := left.(*ast.Ident)
		if !ok {
			continue
		}
		if typeName := r.typeOf(assign.Rhs[index]); typeName != "" {
			r.idents[ident.Name] = typeName
		}
	}
}

// typeOf resolves an expression to a package-local struct type, following
// named and promoted fields.
func (r *ownershipResolver) typeOf(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return r.idents[value.Name]
	case *ast.ParenExpr:
		return r.typeOf(value.X)
	case *ast.UnaryExpr:
		return r.typeOf(value.X)
	case *ast.SelectorExpr:
		base := r.typeOf(value.X)
		if base == "" {
			return ""
		}
		typeName, _ := r.field(base, value.Sel.Name, map[string]bool{})
		return typeName
	}
	return ""
}

// field resolves name on typeName, returning the field's type and the struct
// that declares it. Embedded types are searched in declaration depth order.
func (r *ownershipResolver) field(typeName, name string, seen map[string]bool) (string, string) {
	if seen[typeName] {
		return "", ""
	}
	seen[typeName] = true
	entry := r.structs[typeName]
	if entry.fields[name] {
		return entry.named[name], typeName
	}
	for _, embedded := range entry.embeds {
		if embedded == name {
			return embedded, typeName
		}
	}
	for _, embedded := range entry.embeds {
		if fieldType, declaring := r.field(embedded, name, seen); declaring != "" {
			return fieldType, declaring
		}
	}
	return "", ""
}

// stateOwner names the service whose private state selector reaches, or "".
func (r *ownershipResolver) stateOwner(selector *ast.SelectorExpr) string {
	base := r.typeOf(selector.X)
	if base == "" {
		return ""
	}
	_, declaring := r.field(base, selector.Sel.Name, map[string]bool{})
	for _, field := range runtimeOwnedState[declaring] {
		if field == selector.Sel.Name {
			return declaring
		}
	}
	return ""
}
