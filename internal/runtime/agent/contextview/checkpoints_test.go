package contextview

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestContextContinuityP4CheckpointProjectionIsBounded(t *testing.T) {
	var checkpoints []agentcontext.TurnCheckpoint
	for turn := uint64(1); turn <= 1000; turn++ {
		checkpoint, err := agentcontext.RenderTurnCheckpoint(agentcontext.CheckpointRenderInput{Turn: turn, Status: agentcontext.CheckpointCompleted, Goal: strings.Repeat("旧任务", 30), Budget: 512})
		if err != nil {
			t.Fatal(err)
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	before := agentcontext.CloneTurnCheckpoints(checkpoints)
	references := []agentcontext.ReferenceCoverage{{SourceTurn: 1}, {SourceTurn: 500}}
	for _, limit := range []int{0, 256, 512, 1024} {
		for _, capacity := range []int{0, 256, 512, 2048} {
			fits := func(messages []provider.Message) (bool, error) {
				bytes := 0
				for _, m := range messages {
					bytes += len(m.Text())
				}
				return bytes <= capacity, nil
			}
			selected, err := SelectCheckpoints(checkpoints, references, limit, fits)
			if err != nil {
				t.Fatal(err)
			}
			size := 0
			for _, message := range selected {
				size += len(message.Text())
				if message.Turn != 1 && message.Turn != 500 && message.Turn != 1000 {
					t.Fatalf("unrelated checkpoint %d", message.Turn)
				}
			}
			if size > capacity || limit > 0 && size > limit {
				t.Fatalf("projection exceeds budget: %d, cap=%d bytes=%d", size, capacity, limit)
			}
			again, err := SelectCheckpoints(checkpoints, references, limit, fits)
			if err != nil || !reflect.DeepEqual(selected, again) {
				t.Fatal("projection is not deterministic")
			}
		}
	}
	if !reflect.DeepEqual(before, checkpoints) {
		t.Fatal("projection changed durable checkpoints")
	}
}
