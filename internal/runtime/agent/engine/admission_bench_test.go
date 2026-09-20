package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// Re-admitting an already admitted history is the per-model-step hot path:
// every step re-runs admission over the whole conversation before sampling.
func BenchmarkAdmitToolResultHistory(b *testing.B) {
	for _, size := range []int{20, 100, 400} {
		b.Run(fmt.Sprintf("results=%d", size), func(b *testing.B) {
			engine, err := newTestEngine(Options{
				ProviderConfig: ProviderConfig{
					Provider: &scriptedProvider{}, Route: testRoute(b),
					MaxOutputTokens: 128,
				},
				ToolConfig: ToolConfig{
					Tools: tool.NewRegistry(nil, tool.NewResultStore(32<<10)),
					Authorize: func(provider.ToolCall) bool {
						return true
					},
				},
			})
			if err != nil {
				b.Fatal(err)
			}
			history := make([]provider.Message, 0, size*2)
			for index := 0; index < size; index++ {
				callID := fmt.Sprintf("call-%d", index)
				history = append(
					history,
					toolCallMessage(1, callID, "exec_command", `{}`),
					toolResultMessage(
						1,
						callID,
						"benchmark payload "+strings.Repeat("x", 300),
					),
				)
			}
			admitted, err := engine.admitToolResultHistory(history)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := engine.admitToolResultHistory(admitted); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
