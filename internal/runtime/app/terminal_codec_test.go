package app

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestEventKindIncludesEveryAgentEvent(t *testing.T) {
	for data, want := range map[protocol.EventData]protocol.EventKind{
		&protocol.AgentSpawnedData{}:     protocol.EventAgentSpawned,
		&protocol.AgentStatusData{}:      protocol.EventAgentStatus,
		&protocol.AgentMessageData{}:     protocol.EventAgentMessage,
		&protocol.AgentIntegrationData{}: protocol.EventAgentIntegration,
	} {
		if got := EventKind(data); got != want {
			t.Fatalf("%T kind = %q want %q", data, got, want)
		}
	}
}
