package revert

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

type fakeReverter struct {
	defaultID string
	restored  []string
	conflicts []string
	err       error
	calls     []string
}

func (f *fakeReverter) DefaultTargetTurnID() (string, error) {
	if f.defaultID == "" {
		return "", errors.New("no turn to revert")
	}
	return f.defaultID, nil
}

func (f *fakeReverter) Revert(
	_ context.Context,
	targetTurnID string,
) ([]string, []string, error) {
	f.calls = append(f.calls, targetTurnID)
	return append([]string{}, f.restored...),
		append([]string{}, f.conflicts...),
		f.err
}

func TestRevertTurnUnavailableWithoutReverter(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := Register(registry, Options{}); err != nil {
		t.Fatal(err)
	}
	var found *tool.Descriptor
	for _, d := range registry.Descriptors(tool.VisibleModel) {
		if d.Name == "revert_turn" {
			copy := d
			found = &copy
			break
		}
	}
	if found == nil || found.Availability != tool.AvailabilityUnavailable ||
		found.UnavailableReason != UnavailableReason {
		t.Fatalf("descriptor = %+v", found)
	}
	result, err := (&Tool{}).Execute(t.Context(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Metadata["error_category"] != "unavailable" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRevertTurnFakeHappyPath(t *testing.T) {
	fake := &fakeReverter{
		defaultID: "turn_last",
		restored:  []string{"a.txt"},
	}
	registry := tool.NewRegistry(nil, nil)
	if err := Register(registry, Options{Reverter: fake}); err != nil {
		t.Fatal(err)
	}
	for _, d := range registry.Descriptors(tool.VisibleModel) {
		if d.Name == "revert_turn" && d.Availability != tool.AvailabilityAvailable {
			t.Fatalf("expected available: %+v", d)
		}
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "revert_turn", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, `"a.txt"`) ||
		!strings.Contains(result.Content, `"turn_last"`) {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "turn_last" {
		t.Fatalf("calls = %+v", fake.calls)
	}
	result, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "revert_turn", Arguments: json.RawMessage(`{"target_turn_id":"turn_x"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, `"turn_x"`) {
		t.Fatalf("explicit = %+v", result)
	}
}
