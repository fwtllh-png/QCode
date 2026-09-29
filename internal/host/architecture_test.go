package host

import (
	_ "embed"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// TestOnlyHostsImportHostPackages keeps dependencies pointing inward: hosts
// compose runtime, persistence, and adapters, never the reverse. Tests are
// included because a lower layer's test importing a host hides a missing seam.
func TestOnlyHostsImportHostPackages(t *testing.T) {
	const hostPrefix = "github.com/fwtllh-png/QCode/internal/host"
	internalRoot := filepath.Clean("..")
	hostRoot := filepath.Join(internalRoot, "host")
	files := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == hostRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, importSpec := range file.Imports {
			imported, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				return err
			}
			if imported == hostPrefix || strings.HasPrefix(imported, hostPrefix+"/") {
				t.Errorf("%s imports host package %s", filepath.ToSlash(path), imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("layering scan found no Go files outside internal/host")
	}
}

func TestWebHostDoesNotDependOnExecutionImplementations(t *testing.T) {
	forbidden := []string{
		"github.com/fwtllh-png/QCode/internal/runtime/agent",
		"github.com/fwtllh-png/QCode/internal/adapter/provider",
		"github.com/fwtllh-png/QCode/internal/security/sandbox",
		"github.com/fwtllh-png/QCode/internal/adapter/tool",
		"github.com/fwtllh-png/QCode/internal/adapter/skill",
		"github.com/fwtllh-png/QCode/internal/runtime/app/extension",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		// Integration fixtures may construct execution implementations; the
		// production host must only submit work through Runtime and wiring.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, importSpec := range file.Imports {
			path, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, prefix := range forbidden {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					t.Errorf("%s imports forbidden execution package %s", entry.Name(), path)
				}
			}
		}
	}
}

// TestWebHostDoesNotScanEventHistory keeps event-history interpretation in
// the Runtime. The host streams events to clients (EventsLimited) and asks the
// Runtime evidence queries; paging ReplayEvents here re-implements Runtime
// projections in the transport.
func TestWebHostDoesNotScanEventHistory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "ReplayEvents" {
				t.Errorf("%s pages Runtime event history; add a Runtime query instead",
					files.Position(call.Pos()))
			}
			return true
		})
	}
}

//go:embed web-operation-exposure.json
var operationExposureContract []byte

type operationExposureRegistry struct {
	Version    int `json:"version"`
	Operations []struct {
		Kind            protocol.OperationKind `json:"operation_kind"`
		Disposition     string                 `json:"disposition"`
		IntentSchema    string                 `json:"web_intent_schema"`
		IdentityBinding string                 `json:"identity_binding"`
		AdmissionPolicy string                 `json:"admission_policy"`
		RequiredSurface string                 `json:"required_surface"`
		Qualification   string                 `json:"qualification"`
	} `json:"operations"`
}

func TestWebOperationExposureClassifiesEveryProtocolOperation(t *testing.T) {
	var registry operationExposureRegistry
	if err := json.Unmarshal(operationExposureContract, &registry); err != nil {
		t.Fatal(err)
	}
	if registry.Version != 1 {
		t.Fatalf("exposure registry version = %d", registry.Version)
	}
	kinds := protocol.OperationKinds()
	if len(webOperationExposure) != len(kinds) ||
		len(registry.Operations) != len(kinds) {
		t.Fatalf(
			"Web operation exposure has code=%d registry=%d entries, protocol has %d",
			len(webOperationExposure), len(registry.Operations),
			len(kinds),
		)
	}
	registered := make(map[protocol.OperationKind]bool, len(registry.Operations))
	for _, entry := range registry.Operations {
		if registered[entry.Kind] {
			t.Fatalf("duplicate operation %q", entry.Kind)
		}
		registered[entry.Kind] = true
		exposed, classified := webOperationExposure[entry.Kind]
		if !classified {
			t.Errorf("registered operation %q is unknown to the Web Host", entry.Kind)
			continue
		}
		if entry.Qualification == "" || entry.IdentityBinding == "" ||
			entry.AdmissionPolicy == "" {
			t.Errorf("operation %q has incomplete qualification metadata", entry.Kind)
		}
		switch entry.Disposition {
		case "exposed":
			if !exposed || entry.IntentSchema == "" || entry.RequiredSurface == "" {
				t.Errorf("operation %q has an incomplete exposed contract", entry.Kind)
			}
		case "denied":
			if exposed || entry.IntentSchema != "" || entry.RequiredSurface != "" ||
				entry.AdmissionPolicy != "deny" {
				t.Errorf("operation %q has an invalid denied contract", entry.Kind)
			}
		default:
			t.Errorf("operation %q has disposition %q", entry.Kind, entry.Disposition)
		}
	}
	for _, kind := range kinds {
		if _, classified := webOperationExposure[kind]; !classified {
			t.Errorf("operation %q has no explicit Web exposure decision", kind)
		}
		if !registered[kind] {
			t.Errorf("operation %q is missing from the exposure registry", kind)
		}
	}
}
