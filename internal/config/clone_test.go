package config

import "testing"

func TestCloneSnapshotCopiesNestedMapsAndSlices(t *testing.T) {
	original := Snapshot{
		Config: Config{
			Route: Route{Slots: map[string]RouteSlot{
				"summary": {Provider: "fixture", Model: "summarizer"},
			}},
			Diagnostics: Diagnostics{Commands: map[string]DiagnosticCommand{
				".go": {Name: "go", Args: []string{"test", "{path}"}},
			}},
			Execution: Execution{Environment: ExecutionEnvironment{
				Resources: []EnvironmentResource{{
					Name: "tool-config", Namespace: "host_config",
					Access: "read", Path: "/opt/a", Methods: []string{"GET"},
				}},
			}},
		},
		Provenance: map[string]Source{fieldProvider: SourceFile},
	}
	cloned := CloneSnapshot(original)
	cloned.Config.Route.Slots["summary"] = RouteSlot{
		Provider: "other",
		Model:    "other",
	}
	command := cloned.Config.Diagnostics.Commands[".go"]
	command.Args[0] = "vet"
	cloned.Config.Diagnostics.Commands[".go"] = command
	cloned.Config.Execution.Environment.Resources[0].Path = "/opt/b"
	cloned.Config.Execution.Environment.Resources[0].Methods[0] = "POST"
	cloned.Provenance[fieldProvider] = SourceStartup

	if original.Config.Route.Slots["summary"].Provider != "fixture" ||
		original.Config.Diagnostics.Commands[".go"].Args[0] != "test" ||
		original.Config.Execution.Environment.Resources[0].Path != "/opt/a" ||
		original.Config.Execution.Environment.Resources[0].Methods[0] != "GET" ||
		original.Provenance[fieldProvider] != SourceFile {
		t.Fatalf("original snapshot mutated through clone: %+v", original)
	}
}
