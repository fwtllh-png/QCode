package toolsearch_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type projectionExecutor struct{ stubExec }

func (s projectionExecutor) Descriptor() tool.Descriptor {
	d := s.stubExec.Descriptor()
	if strings.HasSuffix(s.name, "_inspect") {
		d.InputSchema["description"] = strings.Repeat("argument documentation ", 80)
	}
	return d
}

func TestProjectionKeepsDiscoveryAfterCapacityTrimming(t *testing.T) {
	for _, capacity := range []string{"definitions", "schema", "exact_fit", "required_overflow"} {
		t.Run(capacity, func(t *testing.T) {
			registry := tool.NewRegistry(nil, nil)
			if err := toolsearch.Register(registry); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"file_read", "exec_command", "alpha_inspect", "beta_inspect", "gamma_inspect"} {
				if err := registry.Register(projectionExecutor{stubExec: stubExec{
					name: name, desc: "inspect",
				}}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := registry.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			selectedBytes := 0
			for _, entry := range snapshot.Entries() {
				if entry.Name != toolsearch.ToolName {
					data, _ := json.Marshal(entry.PresentationDescriptor().InputSchema)
					selectedBytes += len(data)
				}
			}
			request := toolsearch.ProjectionRequest{
				Catalog: snapshot, Prompt: "inspect",
				MaxDefinitions: 6, MaxSchemaBytes: selectedBytes,
			}
			switch capacity {
			case "definitions":
				request.MaxDefinitions = 5
			case "schema":
				request.MaxSchemaBytes--
			case "required_overflow":
				request.MaxDefinitions = 3
			}
			definitions, advertised, err := toolsearch.ProjectDefinitions(request)
			if capacity == "required_overflow" {
				if !errors.Is(err, tool.ErrCatalogLimit) {
					t.Fatalf("missing required discovery must fail explicitly: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if capacity == "exact_fit" {
				if advertised[toolsearch.ToolName] || len(definitions) != 6 {
					t.Fatalf("unnecessary discovery displaced an exact fit: %v", advertised)
				}
				return
			}
			if !advertised[toolsearch.ToolName] || advertised["gamma_inspect"] {
				t.Fatalf("trimmed tool lost its discovery entry point: %v", advertised)
			}
			for _, name := range []string{"file_read", "exec_command", "result_get"} {
				if !advertised[name] {
					t.Fatalf("required tool %s displaced: %v", name, advertised)
				}
			}
			result, err := tooltest.Execute(t.Context(), registry, tool.Call{
				Name: toolsearch.ToolName, Arguments: json.RawMessage(`{"query":"gamma_inspect","limit":1}`),
			})
			if err != nil || result.IsError {
				t.Fatalf("discover omitted tool: %+v %v", result, err)
			}
			request.Catalog, err = registry.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			_, advertised, err = toolsearch.ProjectDefinitions(request)
			if err != nil || !advertised["gamma_inspect"] || !advertised[toolsearch.ToolName] {
				t.Fatalf("next sample did not expose recovered tool: %v %v", advertised, err)
			}
		})
	}
}
