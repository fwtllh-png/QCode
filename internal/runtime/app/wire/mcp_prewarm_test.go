package wire

import (
	"context"
	"testing"
	"time"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
)

func TestMCPPrewarmOutlivesTheContextThatStartedIt(t *testing.T) {
	prewarm := NewMCPPrewarmConfig(mcpruntime.NewPool(nil), mcpruntime.Config{})
	prewarm.loadConfig = nil
	request, cancel := context.WithCancel(context.Background())
	prewarm.Start(request)
	t.Cleanup(prewarm.Stop)
	cancel()

	prewarm.RequestRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for prewarm.dirty.Load() {
		if time.Now().After(deadline) {
			t.Fatal("prewarm worker stopped with the request that constructed it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
