package wire

import (
	"log/slog"
	"os"
	"sync"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/memory"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	interacttool "github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/orchestration/childrun"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/joblog"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type providerBundle struct {
	fixture           *fixture.Server
	providerID        string
	modelID           string
	modelCapabilities protocol.ModelCapabilities
	providerCatalog   protocol.ProviderCatalog
	modelCatalog      protocol.ModelCatalog
}

type platformBundle struct {
	processes           *process.SessionManager
	workspaceQuery      *workspacequery.Service
	repositoryIndex     *repoindex.Index
	sandbox             sandbox.Backend
	environmentStateDir string
}

type capabilityBundle struct {
	mcpPool    *mcpruntime.Pool
	mcpPrewarm *MCPPrewarm
	memory     *memory.Store
}

type orchestrationBundle struct {
	inputHost        *interacttool.Host
	applyPlan        func(interacttool.Plan) error
	children         *childrun.Runner
	childTools       *childToolsets
	chatWorkspaces   *chatWorkspaces
	subagents        *subagent.AgentControl
	turnCoordinators *durableCoordinatorRuntime
}

type persistenceBundle struct {
	content           *contentstore.Memory
	jobLogs           *joblog.Store
	ephemeralState    *sqlitestate.Store
	ephemeralStateDir string
	journal           *workspacejournal.Manager
	journalRecovery   workspacejournal.Recovery
}

type securityBundle struct {
	security           *securitypolicy.Runtime
	Constitution       securitypolicy.ConstitutionStatus
	constitutionPrompt string
}

type observabilityBundle struct {
	metrics     *telemetry.Metrics
	metricsPath string
	logger      *slog.Logger
	logFile     *os.File
	traces      traceSession
}

type runtimeBundle struct {
	Runtime *app.Runtime
	threads *app.ThreadManager
}

type resourceBundle struct {
	resources *ResourceStack
	closeOnce sync.Once
	closeErr  error
}
