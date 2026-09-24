package wire

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type ExecOptions struct {
	ConfigPath          string
	ConfigOverrides     config.Overrides
	BaseURL             string
	APIKeyEnv           string
	FixturePath         string
	Permission          string
	RepositoryRulesPath string
	MCPConfigPath       string
	MetricsPath         string
	LogPath             string
	ModelMetadata       ModelMetadataOptions
	WorkingSet          []ContextFile
	PromptBudgets       map[string]promptcontext.Budget
	PersistentStore     *state.Store
	CredentialControl   *credential.Control
	Skills              SkillOptions
	// TrustProbe lets probe "supported" observations widen catalog capabilities.
	// Without it, probes may only tighten.
	TrustProbe bool
	// ForceEditPlanApproval is enabled by interactive editor hosts that must
	// preview every workspace write regardless of broader policy grants.
	ForceEditPlanApproval bool
	// WorkspaceIdentity binds editor-visible URI identity for editor hosts.
	// Non-editor hosts leave it empty and retain local file URI behavior.
	WorkspaceIdentity protocol.WorkspaceIdentity
	// SkipHostGoproxyAuth keeps tests from binding the machine GOPROXY
	// credential onto the process session protocol handler.
	SkipHostGoproxyAuth bool
	// ExtraConnections 注入默认连接之外的可选模型路由：每条连接带各自的
	// 凭证引用与模型清单，进入 SelectableRoutes 与模型目录，会话可按
	// (provider, model) 热切换。默认连接仍承担 act 路由与凭证热轮换。
	ExtraConnections []ExtraConnectionSpec
}

// ExtraConnectionSpec 描述一条附加模型连接：显式 Base URL、协议、
// 凭证引用与基线/附加模型描述，进入 SelectableRoutes 与模型目录，
// 会话可按 (provider, model) 热切换。
type ExtraConnectionSpec struct {
	ProviderID string
	BaseURL    string
	Protocol   model.WireProtocol
	Credential model.CredentialRef
	Model      *model.Model
	Models     map[string]model.Model
}

// ContextFile is a file a host named for the session (`exec --file`, an editor
// attachment). Its contents go into the prompt once at bootstrap, and its path
// stays in the working set for the rest of the session.
//
// Hosts express this in wire's own vocabulary rather than the agent's, because a
// host must not depend on execution packages.
type ContextFile struct {
	Path string
	// Content replaces what is read from disk, for a buffer the user has not
	// saved. Nil means read the file.
	Content *string
	// Critical pins the path: it never decays out of the working set.
	Critical bool
}

type Session struct {
	providerBundle
	platformBundle
	capabilityBundle
	orchestrationBundle
	persistenceBundle
	securityBundle
	observabilityBundle
	runtimeBundle
	resourceBundle
	configuration sessionConfiguration
}

func NewExec(ctx context.Context, options ExecOptions) (_ *Session, resultErr error) {
	return newExec(ctx, options, defaultBuildModules())
}

func defaultBuildModules() []buildModule {
	return []buildModule{
		configModule{},
		providerModule{},
		persistenceModule{},
		platformModule{},
		builtinToolsModule{},
		capabilityToolsModule{},
		securityModule{},
		orchestrationModule{},
		observabilityModule{},
		agentModule{},
		runtimeModule{},
		backgroundModule{},
	}
}

func newExec(
	ctx context.Context,
	options ExecOptions,
	modules []buildModule,
) (_ *Session, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session := &Session{
		observabilityBundle: observabilityBundle{
			metrics: telemetry.NewMetrics(), metricsPath: options.MetricsPath,
		},
		resourceBundle: resourceBundle{
			resources: NewResourceStack(),
		},
	}
	if err := session.registerResourceClosers(); err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, session.resources.Close(context.Background()))
		}
	}()
	state := &buildState{options: options, session: session}
	if err := buildModules(ctx, state, modules...); err != nil {
		return nil, err
	}
	return session, nil
}

func configuredDiagnosticCommands(
	configured map[string]config.DiagnosticCommand,
) map[string]diagnostics.Command {
	commands := make(map[string]diagnostics.Command, len(configured))
	for extension, command := range configured {
		commands[extension] = diagnostics.Command{
			Name: command.Name,
			Args: append([]string(nil), command.Args...),
		}
	}
	return commands
}

func diagnosticCommandReadRoots(
	commands map[string]diagnostics.Command,
) []string {
	seen := make(map[string]struct{})
	addExecutableTree := func(name string, includePackageRoot bool) {
		path, err := exec.LookPath(name)
		if err != nil {
			return
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return
		}
		root := filepath.Dir(resolved)
		if includePackageRoot {
			root = filepath.Dir(root)
		}
		seen[root] = struct{}{}
	}
	for _, command := range commands {
		path, err := exec.LookPath(command.Name)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		seen[filepath.Dir(resolved)] = struct{}{}
		for _, dependency := range diagnosticDependencyReadFiles(resolved) {
			seen[filepath.Dir(dependency)] = struct{}{}
		}
		switch filepath.Ext(resolved) {
		case ".js", ".cjs", ".mjs":
			addExecutableTree("node", true)
			node, nodeErr := exec.LookPath("node")
			if nodeErr == nil {
				for _, dependency := range diagnosticDependencyReadFiles(node) {
					seen[filepath.Dir(dependency)] = struct{}{}
				}
			}
		}
	}
	roots := make([]string, 0, len(seen))
	for root := range seen {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return roots
}

func diagnosticCommandReadFiles(
	commands map[string]diagnostics.Command,
) []string {
	seen := make(map[string]struct{})
	add := func(name string) {
		path, err := exec.LookPath(name)
		if err != nil {
			return
		}
		for _, dependency := range diagnosticDependencyReadFiles(path) {
			seen[dependency] = struct{}{}
		}
	}
	for _, command := range commands {
		add(command.Name)
		path, err := exec.LookPath(command.Name)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			switch filepath.Ext(resolved) {
			case ".js", ".cjs", ".mjs":
				add("node")
			}
		}
	}
	files := make([]string, 0, len(seen))
	for path := range seen {
		files = append(files, path)
	}
	slices.Sort(files)
	return files
}

func cloneThreadSecurity(source *policy.Runtime) *policy.Runtime {
	if source == nil {
		return nil
	}
	cloned := source.CloneSampling()
	cloned.Approvals = policy.NewApprovalCache()
	return cloned
}

// childEngineOptions drops the host InputHost, journal and turn gate unless the
// child is serialized into the same workspace, where the gate makes reuse safe.
func childEngineOptions(
	seed agentengine.Options,
	spec app.ChildSpec,
) agentengine.Options {
	options := seed
	options.Guard = nil
	options.InputHost = nil
	options.ReadTracker = nil
	options.Security = cloneThreadSecurity(seed.Security)
	if !spec.Serialized {
		options.Journal = nil
		options.WorkspaceTurnGate = nil
	} else if spec.ReadOnly {
		options.Journal = nil
	} else {
		options.ReadTracker = workspacejournal.NewReadTracker()
	}
	if spec.MaxSteps > 0 {
		options.MaxSteps = spec.MaxSteps
	}
	if spec.MaxTokens > 0 {
		options.Budget.MaxTokens = spec.MaxTokens
		options.Budget.MaxTurnTokens = spec.MaxTokens
	}
	if spec.MaxCostUSD > 0 {
		options.Budget.MaxCostUSD = spec.MaxCostUSD
	}
	if spec.SessionID != "" {
		options.SessionID = spec.SessionID
	}
	if spec.Workspace != "" {
		options.Workspace = spec.Workspace
		options.WorkspaceIsolation = app.SessionIsolationWorktree
	}
	if spec.ReadOnly && !spec.CanDelegate && options.Security != nil {
		// Plan mode is the existing, tested read-only enforcement: everything
		// that is not a read capability is denied with mode_denied. A read-only
		// stance that only shaped the prompt would not be a stance at all.
		options.Security.SetModePermission(policy.ModePlan, policy.PermissionNever)
	}
	if slices.Contains(spec.AllowedTools, "verify") {
		options.VerificationOnly = true
	}
	if options.Security != nil {
		options.ProfilePermissionCeiling = options.Security.PermissionValue()
	}
	return options
}

func restrictChildTools(
	security *policy.Runtime,
	spec app.ChildSpec,
	parent, child *tool.Registry,
) {
	if security == nil || child == nil {
		return
	}
	parentTools := make(map[string]struct{})
	if parent != nil {
		for _, descriptor := range allToolDescriptors(parent) {
			parentTools[descriptor.Name] = struct{}{}
		}
	}
	for _, descriptor := range allToolDescriptors(child) {
		_, inherited := parentTools[descriptor.Name]
		if inherited && childRoleAllowsTool(spec, descriptor) {
			continue
		}
		_, _ = security.AppendManagedRule(policy.Rule{
			Tool: descriptor.Name, Resource: "*", Action: policy.ActionDeny,
			Code: "child_authority_denied",
		})
	}
}

func allToolDescriptors(registry *tool.Registry) []tool.Descriptor {
	var result []tool.Descriptor
	for _, visibility := range []tool.Visibility{
		tool.VisibleModel, tool.VisibleInternal, tool.VisibleHidden,
	} {
		result = append(result, registry.Descriptors(visibility)...)
	}
	return result
}

func childRoleAllowsTool(spec app.ChildSpec, descriptor tool.Descriptor) bool {
	if isAgentLifecycleTool(descriptor.Name) {
		return spec.CanDelegate
	}
	if len(spec.AllowedTools) == 0 {
		return true
	}
	for _, allowed := range spec.AllowedTools {
		switch allowed {
		case "read", "search":
			if descriptor.Capability == tool.CapabilityRead {
				return true
			}
		case "process.read_only":
			if descriptor.Capability == tool.CapabilityProcess &&
				descriptor.AccessMode == tool.AccessRead &&
				!mutatingProcessTool(descriptor.Name) {
				return true
			}
		case "verify":
			if descriptor.Name == "exec_command" || descriptor.Name == "write_stdin" {
				return true
			}
		case descriptor.Name:
			return true
		}
	}
	return descriptor.Name == "turn_complete" || descriptor.Name == "result_get"
}

func mutatingProcessTool(name string) bool {
	switch name {
	case "exec_command", "write_stdin":
		return true
	default:
		return false
	}
}

func isAgentLifecycleTool(name string) bool {
	switch name {
	case "spawn_agent", "send_message", "wait_agent", "list_agents",
		"followup_task", "interrupt_agent", "close_agent", "integrate_agent":
		return true
	default:
		return false
	}
}
