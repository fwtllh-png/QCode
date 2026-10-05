package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func BenchmarkSnapshotNormalize(b *testing.B) {
	for _, fixture := range []struct {
		name       string
		messages   int
		imageBytes int
	}{
		{name: "text_800", messages: 800},
		{name: "images_8x256KiB", messages: 8, imageBytes: 256 << 10},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			history := make([]provider.Message, fixture.messages)
			for index := range history {
				history[index] = provider.TextMessage(provider.RoleUser, strings.Repeat("x", 400))
				if fixture.imageBytes != 0 {
					history[index].Blocks = append(history[index].Blocks, provider.ContentBlock{
						Type: provider.ContentImage,
						Attachment: &provider.Attachment{
							Name: "screen.png", MediaType: "image/png", Data: make([]byte, fixture.imageBytes),
						},
					})
				}
			}
			snapshot := NewMessageLedger(LedgerInput{History: history}).Snapshot()
			capabilities := model.Capabilities{ImageInput: true, Reasoning: true, ToolCalls: true}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, _, err := snapshot.Normalize(capabilities); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
