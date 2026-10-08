package contextview

import (
	"slices"
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// SelectCheckpoints projects only referenced turns and the most recent closed
// turn. The durable records are unchanged. fits measures the complete request,
// so optional checkpoints cannot displace the current task or its definitions.
func SelectCheckpoints(checkpoints []agentcontext.TurnCheckpoint, references []agentcontext.ReferenceCoverage, maxBytes int, fits func([]provider.Message) (bool, error)) ([]provider.Message, error) {
	wanted := make(map[uint64]bool)
	var latest uint64
	for _, checkpoint := range checkpoints {
		latest = max(latest, checkpoint.Turn)
	}
	wanted[latest] = true
	for _, reference := range references {
		wanted[reference.SourceTurn] = true
	}
	ordered := slices.Clone(checkpoints)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Turn > ordered[j].Turn })
	var selected []provider.Message
	bytes := 0
	for _, checkpoint := range ordered {
		if !wanted[checkpoint.Turn] || maxBytes > 0 && len(checkpoint.Text) > maxBytes-bytes {
			continue
		}
		candidate := append(slices.Clone(selected), agentcontext.CheckpointMessages([]agentcontext.TurnCheckpoint{checkpoint})...)
		ok, err := fits(candidate)
		if err != nil {
			return nil, err
		}
		if ok {
			selected = candidate
			bytes += len(checkpoint.Text)
		}
	}
	slices.Reverse(selected)
	return selected, nil
}
