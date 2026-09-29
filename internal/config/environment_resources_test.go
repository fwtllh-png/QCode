package config

import (
	"testing"
)

func TestExecutionEnvironmentLoadsDeclaredResources(t *testing.T) {
	path := writeConfig(t, `
[execution.environment]
contract = "v1"
profile = "native"

[[execution.environment.resources]]
name = "tool-config"
namespace = "host_config"
access = "read"
path = "/opt/tool/config.toml"

[[execution.environment.resources]]
name = "tool-cache"
namespace = "cache"
access = "write"
path = "sandbox-home/cache/tool"
env = "TOOL_CACHE"
tree = true
`)
	snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Config.Execution.Environment
	if snapshot.Provenance[fieldEnvironmentResources] != SourceFile ||
		len(got.Resources) != 2 ||
		got.Resources[0].Path != "/opt/tool/config.toml" ||
		got.Resources[1].Env != "TOOL_CACHE" {
		t.Fatalf("declared resources = %+v provenance=%q", got.Resources, snapshot.Provenance[fieldEnvironmentResources])
	}
	requests := got.DeclaredRequests()
	if len(requests) != 2 ||
		requests[0].Source != "user-declaration" ||
		requests[1].Env != "TOOL_CACHE" {
		t.Fatalf("declared requests = %+v", requests)
	}
}
