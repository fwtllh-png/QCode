package agentcontext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

// One full attribution pass over a snapshot is the per-attempt hot path:
// compaction gates, sample measurement, and prefix accounting all lean on it.
func BenchmarkSnapshotMeasure(b *testing.B) {
	estimate := EstimatorFunc(func(messages []provider.Message) (uint64, error) {
		return EstimateMessageTokens(messages), nil
	})
	for _, size := range []int{50, 200, 800} {
		b.Run(fmt.Sprintf("messages=%d", size), func(b *testing.B) {
			history := make([]provider.Message, size)
			for index := range history {
				role := provider.RoleUser
				if index%2 != 0 {
					role = provider.RoleAssistant
				}
				history[index] = provider.TextMessage(
					role,
					strings.Repeat("x", 400),
				)
			}
			snapshot := NewMessageLedger(LedgerInput{
				Stable: []provider.Message{
					provider.TextMessage(provider.RoleSystem, strings.Repeat("s", 400)),
				},
				History: history,
			}).Snapshot()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := snapshot.Measure("bench", "high", estimate); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
