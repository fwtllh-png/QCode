package wire

import (
	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/memory"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/admission"
	workbudget "github.com/fwtllh-png/QCode/internal/orchestration/budget"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/joblog"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/constitution"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/permissions"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type providerBuildState struct {
	routes            model.RouteSet
	route             model.ReadyRoute
	selectableRoutes  map[string]model.ReadyRoute
	egress            *egress.Gate
	provider          provider.Provider
	toolSampler       *agentengine.ToolSampler
	providerCatalog   protocol.ProviderCatalog
	modelCatalog      protocol.ModelCatalog
	modelCapabilities protocol.ModelCapabilities
}

type platformBuildState struct {
	helperPath      string
	backend         sandbox.Backend
	web             webtool.Options
	webEgress       *egress.Gate
	processEgress   *egress.Gate
	processes       *process.SessionManager
	leaseAuthority  *toolguard.LeaseAuthority
	repositoryIndex *repoindex.Index
}

type persistenceBuildState struct {
	content        *contentstore.Memory
	jobLogs        *joblog.Store
	runtimeStore   *sqlitestate.Store
	ephemeralState *sqlitestate.Store
}

type capabilityBuildState struct {
	skillCatalog *skill.Catalog
	memory       *memory.Store
	mcpPool      *mcpruntime.Pool
	mcpPrewarm   *MCPPrewarm
}

type securityBuildState struct {
	runtime      *policy.Runtime
	journal      *workspacejournal.Manager
	constitution constitution.Bundle
	permissions  *permissions.Store
	guardFactory guardFactory
	diagnostics  diagnostics.Runner
	verify       verify.Runner
	guard        *toolguard.Guard
}

type orchestrationBuildState struct {
	workBudget    *workbudget.Ledger
	childGovernor *admission.Governor
	children      *childRuntime
	childToolsets *childToolsets
	chatTrees     *childWorktrees
	parentFiles   *filetool.Tools
	subagents     *subagent.AgentControl
}
