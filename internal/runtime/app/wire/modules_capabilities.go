package wire

import (
	"context"
	"errors"
	"fmt"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type capabilityToolsModule struct{}

func (capabilityToolsModule) Name() string { return "capability-tools" }

func (capabilityToolsModule) Build(
	ctx context.Context,
	state *buildState,
) error {
	if !state.config.execution.Tools {
		return nil
	}
	registry := state.tools.registry
	if registry == nil {
		return errors.New("shared tool registry is required")
	}
	output := &state.capabilities
	sandboxPolicy, _ := sandbox.BackendPolicy(state.platform.backend)
	if err := (skillContributor{
		paths: state.config.skillPaths, workspace: state.config.execution.Workspace,
		sandboxHome: sandboxPolicy.PrivateTemp, output: output,
	}).Contribute(ctx, registry); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if err := contributeMemory(
		ctx, registry, state.config.snapshot.Config.Memory, output,
	); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	var mcpAuthority *mcpruntime.RuntimeAuthority
	if state.options.MCPConfigPath != "" {
		var err error
		mcpAuthority, err = mcpruntime.NewRuntimeAuthority(
			state.config.execution.Workspace,
			state.config.workspaceStateID,
			1,
			state.platform.backend,
			state.platform.leaseAuthority,
		)
		if err != nil {
			return fmt.Errorf("MCP authority: %w", err)
		}
	}
	if err := (mcpContributor{
		configPath: state.options.MCPConfigPath, trustedConfigRoot: state.config.skillPaths.DataDir,
		runtimeAuthority: mcpAuthority, output: output,
	}).Contribute(ctx, registry); err != nil {
		return fmt.Errorf("MCP: %w", err)
	}
	publishCapabilityOutputs(state)
	return nil
}
