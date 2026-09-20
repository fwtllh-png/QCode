package permissions

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func snapshotInvocation(host string) policy.Invocation {
	return policy.Invocation{
		CallID: host, Tool: "web_fetch", Capability: tool.CapabilityNetwork,
		Access: tool.AccessRead, Sandbox: tool.SandboxNone, Validated: true,
		Resources: []tool.Resource{{Kind: "host", ID: host, Access: tool.AccessRead}},
	}
}

func TestStorePublishesOnlyDurableVersions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	before, version := store.UserRulesSince(0)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.SetDisableAutoReview(true)
	runtime.BindUserRuleSource(store)
	first := snapshotInvocation("first.example")
	if _, err := store.AppendAllow(first); err != nil {
		t.Fatal(err)
	}
	published, current := store.UserRulesSince(version)
	if len(before) != 0 || len(published) != 1 || current <= version {
		t.Fatalf("snapshot before=%v published=%v versions=%d/%d", before, published, version, current)
	}
	if got := runtime.Evaluate(first); got.Action != policy.ActionAllow {
		t.Fatalf("persisted permission unavailable: %+v", got)
	}
	if _, err := store.AppendAllow(first); err != nil {
		t.Fatal(err)
	}
	if rules, same := store.UserRulesSince(current); rules != nil || same != current {
		t.Fatalf("duplicate grant republished rules=%v version=%d", rules, same)
	}
	// Returned slices cannot mutate the published authority.
	published[0].Action = policy.ActionDeny
	if got := runtime.Evaluate(first); got.Action != policy.ActionAllow {
		t.Fatalf("snapshot caller mutated authority: %+v", got)
	}
	reopened, err := OpenStore(store.Path)
	if err != nil || len(reopened.Rules()) != 1 {
		t.Fatalf("durable permission: store=%v err=%v", reopened, err)
	}
	store.Path = t.TempDir() // A directory cannot be replaced as a permission file.
	if _, err := store.AppendAllow(snapshotInvocation("failed.example")); err == nil {
		t.Fatal("expected persistence failure")
	}
	if rules, same := store.UserRulesSince(current); rules != nil || same != current {
		t.Fatal("failed persistence published permission")
	}
	if got := runtime.Evaluate(snapshotInvocation("failed.example")); got.Action != policy.ActionAsk {
		t.Fatalf("failed persistence became usable: %+v", got)
	}
}

func TestStoreConcurrentPolicySnapshots(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
			runtime.BindUserRuleSource(store)
			call := snapshotInvocation(fmt.Sprintf("host-%d.example", index))
			for range count {
				_ = runtime.CloneSampling()
			}
			if _, err := store.AppendAllow(call); err != nil {
				t.Error(err)
				return
			}
			if got := runtime.Evaluate(call); got.Action != policy.ActionAllow {
				t.Errorf("published permission unavailable: %+v", got)
			}
		})
	}
	group.Wait()
	if len(store.Rules()) != count {
		t.Fatalf("lost concurrent permissions: %d", len(store.Rules()))
	}
}
