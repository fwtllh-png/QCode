package policy

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestAuthoritySourcePriorityCannotOverrideHigherDeny(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	runtime.Grants = []Rule{{
		Tool: "run_command", Action: ActionDeny,
	}}
	if _, err := runtime.ReloadSources(
		[]Rule{{Tool: "run_command", Action: ActionAllow}},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	decision := runtime.Decide(resolveFixture(processInvocation("git status")))
	if decision.Action != ActionDeny || decision.Code != "tool_grant_denied" {
		t.Fatalf("decision = %+v", decision)
	}

	runtime.Grants = []Rule{{Tool: "*", Resource: "*", Action: ActionAllow}}
	if _, err := runtime.ReloadSources(
		[]Rule{{Tool: "run_command", Action: ActionAllow}},
		[]Rule{{Tool: "run_command", Action: ActionDeny}},
	); err != nil {
		t.Fatal(err)
	}
	decision = runtime.Decide(resolveFixture(processInvocation("git status")))
	if decision.Action != ActionDeny || decision.Code != "repository_rule_denied" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestReloadSourcesPublishesWholeSnapshots(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	call := processInvocation("git status")
	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				decision := runtime.Decide(resolveFixture(call))
				if decision.Action != ActionAllow && decision.Action != ActionDeny &&
					decision.Action != ActionAsk {
					t.Errorf("partial decision = %+v", decision)
					return
				}
			}
		}()
	}
	for iteration := 0; iteration < 100; iteration++ {
		repository := []Rule(nil)
		if iteration%2 == 0 {
			repository = []Rule{{Tool: "run_command", Action: ActionDeny}}
		}
		if _, err := runtime.ReloadSources(
			[]Rule{{Tool: "run_command", Action: ActionAllow}},
			repository,
		); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
}

func TestReloadSourcesIsAtomicAndRevisioned(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	revision, err := runtime.ReloadSources(
		[]Rule{{Tool: "run_command", Action: ActionAllow}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	old := runtime.CloneSampling()
	if revision != old.Revision {
		t.Fatalf("revision = %d snapshot=%d", revision, old.Revision)
	}
	if _, err := runtime.ReloadSources(nil, []Rule{{
		Tool: "run_command", Action: ActionAllow,
	}}); err == nil {
		t.Fatal("repository allow was accepted")
	}
	current := runtime.CloneSampling()
	if current.Revision != revision || len(current.User) != 1 {
		t.Fatalf("failed reload changed snapshot: %+v", current)
	}
	if _, err := runtime.ReloadSources(nil, []Rule{{
		Tool: "run_command", Action: ActionDeny,
	}}); err != nil {
		t.Fatal(err)
	}
	if old.Revision != revision || len(old.User) != 1 || len(old.Repository) != 0 {
		t.Fatalf("old snapshot changed after reload: %+v", old)
	}
}

func TestValidateRulesRejectsUnsafePersistentPrefix(t *testing.T) {
	for _, prefix := range []string{"bash", "python3 script.py", "git", "rm"} {
		err := ValidateRules(SourceUser, []Rule{{
			Tool: "run_command", CommandPrefix: prefix, Action: ActionAllow,
		}})
		if err == nil {
			t.Fatalf("unsafe prefix accepted: %q", prefix)
		}
	}
	if err := ValidateRules(SourceUser, []Rule{{
		Tool: "run_command", CommandPrefix: "git status", Action: ActionAllow,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvertisesToolHidesBlanketDenyAndKeepsPrefixDeny(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	if !runtime.AdvertisesTool("run_command") {
		t.Fatal("default grant hid run_command")
	}
	if _, err := runtime.AppendManagedRule(Rule{
		Tool: "run_command", Resource: "*", Action: ActionDeny,
	}); err != nil {
		t.Fatal(err)
	}
	if runtime.AdvertisesTool("run_command") {
		t.Fatal("blanket deny still advertised run_command")
	}

	prefixed := DefaultRuntime(ModeAct, PermissionSuggest)
	if _, err := prefixed.AppendManagedRule(Rule{
		Tool: "run_command", CommandPrefix: "rm", Action: ActionDeny,
	}); err != nil {
		t.Fatal(err)
	}
	if !prefixed.AdvertisesTool("run_command") {
		t.Fatal("command-prefix deny hid the whole run_command catalog entry")
	}
}

func processInvocation(command string) invocationFixture {
	arguments, _ := json.Marshal(map[string]string{"command": command})
	return invocationFixture{
		CallID: "call", Tool: "run_command", Arguments: arguments,
		Capability: tool.CapabilityProcess, Access: tool.AccessRead,
		Sandbox: tool.SandboxStrong, Validated: true,
		Resources: []tool.Resource{{
			Kind: "process", ID: "workspace", Access: tool.AccessRead,
		}},
	}
}
