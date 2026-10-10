package shell

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func TestOnlyExecCommandDeclaresVerification(t *testing.T) {
	manager := process.NewSessionManager(128 << 10)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]string{
		"exec_command": "verification",
		"write_stdin":  "",
	} {
		_, _, executor, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		binding := executor.(tool.TrustedBindingProvider).TrustedBinding()
		if binding.IsolatesWriteTrees != (name == "exec_command") {
			t.Fatalf("%s isolation contract = %v", name, binding.IsolatesWriteTrees)
		}
		if binding.GuardianReview != (name == "exec_command") {
			t.Fatalf("%s Guardian registration = %v", name, binding.GuardianReview)
		}
		if err := binding.Validate(); err != nil {
			t.Fatalf("%s binding: %v", name, err)
		}
	}
}
