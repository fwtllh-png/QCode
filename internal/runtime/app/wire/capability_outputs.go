package wire

import (
	"context"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/memory"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
)

func contributeMemory(
	ctx context.Context,
	registry *tool.Registry,
	configuration config.Memory,
	output *capabilityBuildState,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !configuration.Enabled {
		return nil
	}
	store, err := memory.Open(configuration.Path, memory.Options{
		MaxCandidates:  configuration.MaxCandidates,
		MaxPromptBytes: configuration.MaxPromptBytes,
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if err := memory.Register(registry, store); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}
	output.memory = store
	return nil
}

func publishCapabilityOutputs(state *buildState) {
	if state == nil || state.session == nil {
		return
	}
	output := &state.capabilities
	state.session.memory = output.memory
	state.session.mcpPool = output.mcpPool
	state.session.mcpPrewarm = output.mcpPrewarm
	state.tools.skillCatalog = output.skillCatalog
}
