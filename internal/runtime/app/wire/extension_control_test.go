package wire

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestExtensionControlIsIdempotentReplayableAndNonBlocking(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeControlSkill(t, workspace, "review")
	paths, err := ResolveSkillPaths(SkillOptions{
		DataDir: filepath.Join(workspace, "data"), UserHome: home,
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	control, err := openSkillControl(paths, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	initial, err := control.Service.Snapshot(
		t.Context(), protocol.ExtensionControlAll,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Extensions) != 5 {
		t.Fatalf("initial projection = %+v", initial.Extensions)
	}
	review := extensionByName(initial.Extensions, "review")
	if review == nil || !review.Enabled {
		t.Fatalf("review projection = %+v", review)
	}
	for _, name := range []string{
		"system-code-review", "system-debugging",
		"system-refactor", "system-test-expansion",
	} {
		item := extensionByName(initial.Extensions, name)
		if item == nil || item.Source != "builtin" || !item.Enabled {
			t.Fatalf("builtin projection %q = %+v", name, item)
		}
	}

	channel, unsubscribe, err := control.Service.Subscribe(1)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	disable := controlOperation(
		"operation-1", protocol.ExtensionActionDisable, "review",
	)
	first, err := control.Service.Submit(t.Context(), disable)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := control.Service.Submit(t.Context(), disable)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Revision != first.Revision {
		t.Fatalf("duplicate result = %+v, first = %+v", duplicate, first)
	}
	conflict := disable
	conflict.Action = protocol.ExtensionActionEnable
	if _, err := control.Service.Submit(t.Context(), conflict); err == nil {
		t.Fatal("conflicting operation ID was accepted")
	}

	enable := controlOperation(
		"operation-2", protocol.ExtensionActionEnable, "review",
	)
	if _, err := control.Service.Submit(t.Context(), enable); err != nil {
		t.Fatal(err)
	}
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatal("extension subscriber did not receive first event")
	}
	// The second event overflowed the size-one channel. Kernel progress above
	// completed and the slow subscriber is disconnected rather than blocking.
	if _, open := <-channel; open {
		t.Fatal("slow extension subscriber remained connected")
	}

	events, more, err := control.Service.Replay(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(events) != 2 {
		t.Fatalf("events = %+v more=%t", events, more)
	}
	replayed, err := protocol.ReduceExtensionControlEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	current, err := control.Service.Snapshot(
		t.Context(), protocol.ExtensionControlAll,
	)
	if err != nil {
		t.Fatal(err)
	}
	currentReview := extensionByName(current.Extensions, "review")
	if len(replayed) != 1 || currentReview == nil ||
		!reflect.DeepEqual(replayed[0], *currentReview) {
		t.Fatalf("replayed = %+v current review = %+v", replayed, currentReview)
	}
	receiptsOperation := controlOperation(
		"receipts-query", protocol.ExtensionActionReceipts, "",
	)
	receipts, err := control.Service.Submit(t.Context(), receiptsOperation)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts.Receipts) != 2 {
		t.Fatalf("receipts = %+v", receipts.Receipts)
	}
	healthOperation := controlOperation(
		"health-query", protocol.ExtensionActionHealth, "",
	)
	health, err := control.Service.Submit(t.Context(), healthOperation)
	if err != nil {
		t.Fatal(err)
	}
	if health.Diagnostics == nil ||
		health.Diagnostics.Metrics.Operations < 7 ||
		health.Diagnostics.Metrics.Duplicates != 1 ||
		health.Diagnostics.Metrics.SubscriberDrops != 1 ||
		len(health.Diagnostics.Alerts) != 2 {
		t.Fatalf("health diagnostics = %+v", health.Diagnostics)
	}
}

func extensionByName(
	extensions []protocol.ExtensionProjection,
	name string,
) *protocol.ExtensionProjection {
	for index := range extensions {
		if extensions[index].Name == name {
			return &extensions[index]
		}
	}
	return nil
}

func controlOperation(
	id string,
	action protocol.ExtensionControlAction,
	name string,
) protocol.ExtensionControlOperation {
	return protocol.ExtensionControlOperation{
		Version: protocol.Version, ID: id,
		Kind: protocol.ExtensionControlSkill, Action: action, Name: name,
		CreatedAt: time.Now().UTC(),
	}
}

func writeControlSkill(t *testing.T, workspace, name string) {
	t.Helper()
	directory := filepath.Join(workspace, ".agents", "skills", name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name +
		"\ndescription: Review code changes.\n---\nInstructions.\n"
	if err := os.WriteFile(
		filepath.Join(directory, "SKILL.md"), []byte(content), 0o600,
	); err != nil {
		t.Fatal(err)
	}
}
