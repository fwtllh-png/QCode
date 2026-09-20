package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestProducedAssistantBindsReplayToContent(t *testing.T) {
	route := responsesRoute(t, true)
	message := ProducedAssistant(
		route,
		[]ContentBlock{{Type: ContentReasoning, Text: "inspect"}},
		3,
		&ReplayState{
			Version: ReplayVersion,
			Data:    json.RawMessage(`{"items":[{"type":"reasoning"}]}`),
		},
	)
	request := ModelRequest{
		Route: route, Messages: []Message{message}, MaxOutputTokens: 128,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}

	request.Messages[0].Blocks[0].Text = "rewritten"
	if err := request.Validate(); err == nil ||
		!strings.Contains(err.Error(), "does not match assistant content") {
		t.Fatalf("rewritten replay error = %v", err)
	}
}

func TestFilterReplayForRouteDropsCrossAdapterAndLegacyState(t *testing.T) {
	route := responsesRoute(t, true)
	message := ProducedAssistant(
		route,
		[]ContentBlock{{
			Type: ContentReasoning, Text: "inspect",
		}},
		1,
		&ReplayState{Version: ReplayVersion, Data: json.RawMessage(`{"items":[]}`)},
	)

	same := FilterReplayForRoute([]Message{message}, route)
	if same[0].Provenance == nil || same[0].Provenance.Replay == nil {
		t.Fatalf("same-adapter replay was dropped: %+v", same[0])
	}
	crossMessage := message
	crossProvenance := *message.Provenance
	crossProvenance.Adapter = model.AdapterID("legacy")
	crossMessage.Provenance = &crossProvenance
	cross := FilterReplayForRoute([]Message{crossMessage}, route)
	if cross[0].Provenance == nil || cross[0].Provenance.Replay != nil {
		t.Fatalf("cross-adapter replay survived: %+v", cross[0])
	}
	if message.Provenance.Replay == nil {
		t.Fatal("filter mutated source history")
	}

	rewritten := message
	rewritten.Blocks = append([]ContentBlock(nil), message.Blocks...)
	rewritten.Blocks[0].Text = "rewritten"
	filtered := FilterReplayForRoute([]Message{rewritten}, route)
	if filtered[0].Provenance.Replay != nil {
		t.Fatalf("rewritten content retained replay: %+v", filtered[0])
	}
}

func TestReplayProvenanceIsAssistantOnlyAndVersioned(t *testing.T) {
	route := responsesRoute(t, true)
	message := ProducedAssistant(
		route,
		[]ContentBlock{{Type: ContentText, Text: "answer"}},
		1,
		&ReplayState{Version: 2, Data: json.RawMessage(`{}`)},
	)
	request := ModelRequest{
		Route: route, Messages: []Message{message}, MaxOutputTokens: 128,
	}
	if err := request.Validate(); err == nil ||
		!strings.Contains(err.Error(), "unsupported replay state version") {
		t.Fatalf("version error = %v", err)
	}

	message.Provenance.Replay = nil
	message.Role = RoleUser
	request.Messages = []Message{message}
	if err := request.Validate(); err == nil ||
		!strings.Contains(err.Error(), "only valid on assistant") {
		t.Fatalf("role error = %v", err)
	}
}
