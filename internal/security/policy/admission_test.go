package policy

import "testing"

func TestProcessAdmissionFencesPolicyAndCurrentUserRules(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	before := runtime.CloneSampling().Revision
	if err := runtime.WithRevision(before, func() error {
		if runtime.mu.TryLock() {
			runtime.mu.Unlock()
			t.Fatal("policy updates can race the process start")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runtime.SetPermission(PermissionNever)
	if err := runtime.WithRevision(before, func() error { t.Fatal("stale permission started"); return nil }); err == nil {
		t.Fatal("old revision accepted")
	}
	store := &PermissionsStore{}
	store.snapshot.Store(&ruleSnapshot{version: 1})
	runtime.BindUserRuleSource(store)
	before = runtime.CloneSampling().Revision
	store.snapshot.Store(&ruleSnapshot{version: 2, rules: []Rule{{Tool: "exec_command", Action: ActionDeny}}})
	if err := runtime.WithRevision(before, func() error { t.Fatal("stale user rules started"); return nil }); err == nil {
		t.Fatal("old rules accepted")
	}
	if err := runtime.WithRevision(runtime.CloneSampling().Revision, func() error {
		if store.mu.TryLock() {
			store.mu.Unlock()
			t.Fatal("user rules publication can race the process start")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
