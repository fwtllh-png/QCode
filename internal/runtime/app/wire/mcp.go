package wire

import (
	"context"
	"errors"
	"fmt"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type mcpContributor struct {
	configPath        string
	trustedConfigRoot string
	runtimeAuthority  *mcpruntime.RuntimeAuthority
	output            *capabilityBuildState
}

func (c mcpContributor) Contribute(
	_ context.Context,
	registry *tool.Registry,
) error {
	combined := mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: make(map[string]mcpruntime.ServerConfig),
	}
	if c.configPath == "" {
		return nil
	}
	if err := sandbox.RequireTrustedConfigFile(
		c.configPath, c.trustedConfigRoot, "MCP config",
	); err != nil {
		return err
	}
	config, err := mcpruntime.LoadConfig(c.configPath)
	if err != nil {
		return fmt.Errorf("MCP tools: %w", err)
	}
	for name, server := range config.Servers {
		combined.Servers[name] = server
	}
	pool, prewarm, err := RegisterMCPConfig(
		registry, combined, c.runtimeAuthority,
	)
	if err != nil {
		return fmt.Errorf("MCP tools: %w", err)
	}
	c.output.mcpPool, c.output.mcpPrewarm = pool, prewarm
	return nil
}

func RegisterMCPConfig(
	registry *tool.Registry,
	config mcpruntime.Config,
	runtimeAuthority *mcpruntime.RuntimeAuthority,
) (*mcpruntime.Pool, *mcpruntime.Prewarm, error) {
	config = mcpruntime.CloneConfig(config)
	if err := config.Validate(); err != nil {
		return nil, nil, err
	}
	if runtimeAuthority == nil {
		return nil, nil, errors.New("MCP runtime authority is required")
	}
	factory := mcpruntime.NewAuthorizedTransportFactory(runtimeAuthority)
	pool := mcpruntime.NewPool(factory)
	prewarm := mcpruntime.NewPrewarm(pool, config)
	prewarm.SetRegistry(registry)
	prewarm.RequestRefresh()
	return pool, prewarm, nil
}
