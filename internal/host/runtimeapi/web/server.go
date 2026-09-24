package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	runtimeview "github.com/fwtllh-png/QCode/internal/host/runtimeapi/view"
	tracestate "github.com/fwtllh-png/QCode/internal/observability/trace"
	usagestate "github.com/fwtllh-png/QCode/internal/observability/usage"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

const (
	webProtocol      = 1
	websocketTimeout = 5 * time.Second
)

type Options struct {
	Assets        fs.FS
	ExpectedHost  string
	Origin        string
	Token         string
	Build         string
	Capacity      Capacity
	PickDirectory func(context.Context, string) (string, bool, error)
	Setup         *SetupOptions
	Workspaces    WorkspaceController
	Models        ModelController
}

type UsageQuery interface {
	QueryAggregates(
		context.Context,
		usagestate.Query,
	) ([]usagestate.Aggregate, error)
	QueryRollup(
		context.Context,
		usagestate.Query,
	) (usagestate.Rollup, error)
}

type RepositorySymbolQuery interface {
	Symbols(
		context.Context,
		repoindex.Query,
	) ([]repoindex.Symbol, repoindex.Snapshot, error)
}

type Dependencies struct {
	Runtime           *app.Runtime
	WorkspaceRoot     string
	WorkspaceIdentity protocol.WorkspaceIdentity
	DefaultProfile    protocol.SessionProfile
	ProviderCatalog   protocol.ProviderCatalog
	ModelCatalog      protocol.ModelCatalog
	Connection        WorkspaceConnection
	MCPHealth         func() []mcp.HealthSnapshot
	Diagnostics       io.Writer
	Usage             UsageQuery
	Agents            *subagent.AgentControl
	SessionWorkspaces app.SessionWorkspaceManager
	Workspace         *workspacequery.Service
	RepositoryIndex   RepositorySymbolQuery
	Credentials       *credential.Service
	ModelProbe        func(context.Context, string) (bool, error)
	Extensions        interface {
		Submit(
			context.Context,
			protocol.ExtensionControlOperation,
		) (protocol.ExtensionControlResult, error)
		Snapshot(
			context.Context,
			protocol.ExtensionControlKind,
		) (protocol.ExtensionControlResult, error)
	}
}

type Server struct {
	assets        fs.FS
	expectedHost  string
	origin        string
	token         string
	build         string
	index         []byte
	capacity      Capacity
	pickDirectory directoryPicker
	handler       http.Handler

	mu                sync.RWMutex
	directoryPickerMu sync.Mutex
	sessionMu         sync.Mutex
	setupMu           sync.Mutex
	dependencies      Dependencies
	setup             *SetupOptions
	workspaceControl  WorkspaceController
	modelControl      ModelController
	workspaces        map[string]Dependencies
	bootProblem       *protocol.Problem
	ready             atomic.Bool
	draining          atomic.Bool
	reconfiguring     atomic.Bool
	connections       atomic.Int32
}

type responseEnvelope struct {
	Version   int               `json:"version"`
	RequestID string            `json:"request_id,omitempty"`
	Result    any               `json:"result,omitempty"`
	Problem   *protocol.Problem `json:"problem,omitempty"`
}

type bootstrapResponse struct {
	ProtocolVersion  int                         `json:"protocol_version"`
	ServerBuild      string                      `json:"server_build"`
	Token            string                      `json:"token"`
	Ready            bool                        `json:"ready"`
	Draining         bool                        `json:"draining"`
	WorkspaceRoot    string                      `json:"workspace_root,omitempty"`
	Workspace        *protocol.WorkspaceIdentity `json:"workspace,omitempty"`
	SetupRequired    bool                        `json:"setup_required,omitempty"`
	WorkspaceCatalog WorkspaceCatalog            `json:"workspace_catalog"`
	Problem          *protocol.Problem           `json:"problem,omitempty"`
}

type authFrame struct {
	Type        string          `json:"type"`
	Token       string          `json:"token"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Cursor      protocol.Cursor `json:"cursor"`
}

type eventFrame struct {
	Type            string            `json:"type"`
	ProtocolVersion int               `json:"protocol_version"`
	SessionID       string            `json:"session_id,omitempty"`
	Sequence        protocol.Cursor   `json:"sequence"`
	Event           *protocol.Event   `json:"event,omitempty"`
	Problem         *protocol.Problem `json:"problem,omitempty"`
}

func New(options Options) (*Server, error) {
	if options.Assets == nil {
		return nil, errors.New("web assets are required")
	}
	if strings.TrimSpace(options.ExpectedHost) == "" {
		return nil, errors.New("expected Host is required")
	}
	index, err := fs.ReadFile(options.Assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read embedded web index: %w", err)
	}
	token := options.Token
	if token == "" {
		token, err = newToken()
		if err != nil {
			return nil, err
		}
	}
	server := &Server{
		assets: options.Assets, expectedHost: options.ExpectedHost,
		origin: options.Origin, token: token, build: options.Build, index: index,
		capacity:      options.Capacity.normalized(),
		pickDirectory: options.PickDirectory,
		setup:         options.Setup, workspaceControl: options.Workspaces,
		modelControl: options.Models,
		workspaces:   make(map[string]Dependencies),
	}
	if server.setup != nil {
		if err := server.setup.validate(); err != nil {
			return nil, fmt.Errorf("web setup: %w", err)
		}
	}
	if server.pickDirectory == nil {
		server.pickDirectory = nativeDirectoryPicker()
	}
	server.handler = server.routes()
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) CapabilityToken() string { return s.token }

// ActivateSupervisor exposes the workspace catalog without inventing a Runtime
// for an unselected directory.
func (s *Server) ActivateSupervisor() {
	s.mu.Lock()
	s.bootProblem = nil
	s.mu.Unlock()
	s.ready.Store(true)
}

func (s *Server) Activate(dependencies Dependencies) error {
	if err := s.activateWorkspace(dependencies, true); err != nil {
		return err
	}
	s.ready.Store(true)
	return nil
}

func (s *Server) AddWorkspace(dependencies Dependencies) error {
	return s.activateWorkspace(dependencies, false)
}

func (s *Server) RemoveWorkspace(workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return errors.New("workspace id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workspaces, workspaceID)
	if s.dependencies.WorkspaceIdentity.RootID == workspaceID {
		s.dependencies = Dependencies{}
		replacementID := ""
		for id := range s.workspaces {
			if replacementID == "" || id < replacementID {
				replacementID = id
			}
		}
		if replacementID != "" {
			s.dependencies = s.workspaces[replacementID]
		}
	}
	return nil
}

func (s *Server) activateWorkspace(
	dependencies Dependencies,
	makeDefault bool,
) error {
	if dependencies.Runtime == nil {
		return errors.New("web Runtime is required")
	}
	if strings.TrimSpace(dependencies.WorkspaceRoot) == "" {
		return errors.New("web workspace root is required")
	}
	if err := dependencies.WorkspaceIdentity.Validate(); err != nil {
		return fmt.Errorf("web workspace identity: %w", err)
	}
	s.mu.Lock()
	workspaceID := dependencies.WorkspaceIdentity.RootID
	s.workspaces[workspaceID] = dependencies
	if makeDefault || s.dependencies.Runtime == nil ||
		s.dependencies.WorkspaceIdentity.RootID == workspaceID {
		s.dependencies = dependencies
	}
	s.bootProblem = nil
	s.mu.Unlock()
	return nil
}

func (s *Server) FailBoot(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.bootProblem = protocol.NewProblem(
		protocol.CodeUnavailable,
		err.Error(),
		false,
		err,
	)
	s.mu.Unlock()
	s.ready.Store(false)
}

func (s *Server) Drain() {
	s.draining.Store(true)
	s.ready.Store(false)
}

func (s *Server) BeginRuntimeReconfiguration() bool {
	return s.reconfiguring.CompareAndSwap(false, true)
}

func (s *Server) EndRuntimeReconfiguration() {
	s.reconfiguring.Store(false)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/api/v1/bootstrap", s.bootstrap)
	mux.HandleFunc("/api/v1/events", s.events)
	mux.HandleFunc("/api/v1/content/", s.content)
	mux.HandleFunc("/api/v1/", s.unary)
	mux.HandleFunc("/", s.static)
	return s.recoverPanics(s.securityHeaders(s.browserFence(mux)))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	status := http.StatusOK
	state := "ready"
	if s.draining.Load() {
		status, state = http.StatusServiceUnavailable, "draining"
	} else if !s.ready.Load() {
		_, problem := s.snapshot()
		if problem != nil {
			status = http.StatusServiceUnavailable
			state = "boot_failed"
		} else if s.setup != nil {
			state = "setup_required"
		} else {
			status = http.StatusServiceUnavailable
			state = "initializing"
		}
	}
	writeJSON(w, status, map[string]any{
		"version": webProtocol,
		"status":  state,
	})
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	dependencies, problem := s.snapshot()
	result := bootstrapResponse{
		ProtocolVersion: webProtocol,
		ServerBuild:     s.build,
		Token:           s.token,
		Ready:           s.ready.Load(),
		Draining:        s.draining.Load(),
		WorkspaceRoot:   dependencies.WorkspaceRoot,
		Problem:         problem,
	}
	if s.workspaceControl != nil {
		if catalog, err := s.workspaceControl.List(r.Context()); err == nil {
			result.WorkspaceCatalog = catalog
		}
	}
	if result.WorkspaceCatalog.Version == 0 {
		result.WorkspaceCatalog = s.workspaceCatalog()
	}
	if dependencies.WorkspaceIdentity.Version != 0 {
		identity := dependencies.WorkspaceIdentity
		result.Workspace = &identity
	} else if !result.Ready && s.setup != nil {
		if s.setup.WorkspaceIdentity.Version != 0 {
			identity := s.setup.WorkspaceIdentity
			result.WorkspaceRoot = s.setup.WorkspaceRoot
			result.Workspace = &identity
		}
		result.SetupRequired = true
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) unary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if len(r.Header.Get("X-QCode-Request-ID")) > s.capacity.MaxIdentityBytes ||
		len(r.Header.Get("Idempotency-Key")) > s.capacity.MaxIdentityBytes {
		writeProblem(w, r, http.StatusBadRequest, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"request identity exceeds the Web API limit",
			false,
			nil,
		))
		return
	}
	if !s.authorized(r) {
		writeProblem(w, r, http.StatusUnauthorized, protocol.NewProblem(
			protocol.CodeUnavailable,
			"web capability token is missing or invalid",
			false,
			nil,
		))
		return
	}
	if media := r.Header.Get("Content-Type"); !strings.HasPrefix(
		strings.ToLower(media),
		"application/json",
	) {
		writeProblem(w, r, http.StatusBadRequest, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"Content-Type must be application/json",
			false,
			nil,
		))
		return
	}
	if r.ContentLength > s.capacity.MaxJSONBodyBytes {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"request body exceeds the Web API limit",
			false,
			nil,
		))
		return
	}
	route := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	contract, registered := unaryRouteContract(route)
	if !registered {
		writeProblem(w, r, http.StatusNotFound, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"unknown Web API endpoint",
			false,
			nil,
		))
		return
	}
	if s.draining.Load() || (contract.RequiresRuntime &&
		(!s.ready.Load() || s.reconfiguring.Load())) {
		writeProblem(w, r, http.StatusServiceUnavailable, protocol.NewProblem(
			protocol.CodeUnavailable,
			"Runtime is not ready or its provider connection is restarting",
			true,
			nil,
		))
		return
	}
	dependencies, _ := s.snapshot()
	if contract.RequiresRuntime {
		workspaceID := strings.TrimSpace(r.Header.Get(workspaceHeader))
		if workspaceID == "" {
			writeProblem(w, r, http.StatusBadRequest, protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"select a ready workspace before using the Runtime",
				false,
				nil,
			))
			return
		}
		var found bool
		dependencies, _, found = s.workspaceSnapshot(workspaceID)
		if !found {
			writeProblem(w, r, http.StatusNotFound, protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"workspace is not registered with this Web Host",
				false,
				nil,
			))
			return
		}
	}
	handler, dispatched := unaryRouteHandler(route)
	if !dispatched {
		writeProblem(w, r, http.StatusNotFound, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"unknown Web API endpoint",
			false,
			nil,
		))
		return
	}
	result, err := handler(s, r, dependencies)
	if err != nil {
		if dependencies.Diagnostics != nil {
			_, _ = fmt.Fprintf(
				dependencies.Diagnostics,
				"qcode: web API %s: %v\n",
				route,
				err,
			)
		}
		writeApplicationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, responseEnvelope{
		Version: webProtocol, RequestID: requestID(r), Result: result,
	})
}

type unaryHandler func(*Server, *http.Request, Dependencies) (any, error)

func (s *Server) systemDescribe(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	return map[string]any{
		"protocol_version": webProtocol,
		"workspace_root":   dependencies.WorkspaceRoot,
		"workspace":        dependencies.WorkspaceIdentity,
		"profile":          dependencies.DefaultProfile,
		"features": []string{
			"sessions", "events", "profiles", "agent_presets", "tools", "mcp_health",
			"workspace", "credentials", "diagnostics", "session_export",
			"trajectory", "trace_query",
		},
	}, nil
}

func (s *Server) systemReadiness(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	return map[string]any{"ready": true, "draining": false}, nil
}

func (s *Server) systemDiagnostics(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	health := []mcp.HealthSnapshot{}
	if dependencies.MCPHealth != nil {
		health = dependencies.MCPHealth()
	}
	runtimeSnapshot := dependencies.Runtime.Snapshot(r.Context())
	activeAgents := 0
	if dependencies.Agents != nil {
		for _, agent := range dependencies.Agents.List(subagent.ListFilter{}) {
			if subagent.OccupiesSlot(agent.Status) {
				activeAgents++
			}
		}
	}
	return map[string]any{
		"version":   1,
		"ready":     s.ready.Load(),
		"draining":  s.draining.Load(),
		"workspace": dependencies.WorkspaceIdentity,
		"runtime":   runtimeSnapshot,
		"runtime_health": map[string]any{
			"active_turns":           runtimeSnapshot.ActiveTurns,
			"active_provider_calls":  runtimeSnapshot.ActiveProviderCalls,
			"active_tool_executions": runtimeSnapshot.ActiveToolExecutions,
			"active_agents":          activeAgents,
			"pending_approvals":      runtimeSnapshot.PendingApprovals,
			"pending_inputs":         runtimeSnapshot.PendingInputs,
			"pending_operations":     runtimeSnapshot.PendingOperations,
			"goroutines":             goruntime.NumGoroutine(),
			"trace": map[string]any{
				"active_source":                 "in_memory_recorder",
				"durable_source":                "terminal_measurement",
				"raw_spans_table_authoritative": false,
			},
		},
		"mcp_health":   health,
		"generated_at": time.Now().UTC(),
	}, nil
}

func (s *Server) mcpHealth(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if dependencies.MCPHealth == nil {
		return []mcp.HealthSnapshot{}, nil
	}
	return dependencies.MCPHealth(), nil
}

func (s *Server) sessionCreate(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id,omitempty"`
		Title     string `json:"title,omitempty"`
		Isolation string `json:"isolation,omitempty"`
		Provider  string `json:"provider,omitempty"`
		Model     string `json:"model,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"Idempotency-Key header is required",
			false,
			nil,
		)
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"session_id is required for retry-safe Web session creation",
			false,
			nil,
		)
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	sessions, err := dependencies.Runtime.ListSessions(
		r.Context(),
		protocol.SessionListQuery{
			WorkspaceRoot: dependencies.WorkspaceRoot,
			Limit:         s.capacity.MaxActiveSessions,
		},
	)
	if err != nil {
		return nil, err
	}
	if len(sessions.Sessions) >= s.capacity.MaxActiveSessions {
		if _, err := dependencies.Runtime.SessionStatus(
			r.Context(),
			request.SessionID,
		); err != nil {
			return nil, protocol.NewProblem(
				protocol.CodeResourceExhausted,
				"active Web session capacity is exhausted",
				true,
				nil,
			)
		}
	}
	return dependencies.Runtime.CreateSession(r.Context(), app.CreateSessionRequest{
		SessionID: request.SessionID, Title: request.Title,
		Isolation: request.Isolation, Provider: request.Provider,
		Model: request.Model, WorkspaceRoot: dependencies.WorkspaceRoot,
		IdempotencyKey: idempotencyKey,
	})
}

func (s *Server) sessionActivate(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string            `json:"session_id"`
		ThreadID  protocol.ThreadID `json:"thread_id,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.ActivateSession(
		r.Context(),
		app.ActivateSessionRequest{
			SessionID: request.SessionID, ThreadID: request.ThreadID,
			WorkspaceRoot: dependencies.WorkspaceRoot,
		},
	)
}

func (s *Server) sessionList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var query protocol.SessionListQuery
	if err := s.decodeRequest(r, &query); err != nil {
		return nil, err
	}
	query.WorkspaceRoot = dependencies.WorkspaceRoot
	return dependencies.Runtime.ListSessions(r.Context(), query)
}

func (s *Server) sessionStatus(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.SessionStatus(r.Context(), request.SessionID)
}

func (s *Server) sessionUpdate(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID        string                         `json:"session_id"`
		ExpectedRevision uint64                         `json:"expected_revision"`
		Patch            protocol.SessionLifecyclePatch `json:"patch"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.UpdateSessionLifecycle(
		r.Context(),
		request.SessionID,
		request.ExpectedRevision,
		request.Patch,
	)
}

func (s *Server) sessionDelete(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID        string `json:"session_id"`
		ExpectedRevision uint64 `json:"expected_revision"`
		Discard          bool   `json:"discard,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.Discard {
		return dependencies.Runtime.DiscardSession(
			r.Context(),
			request.SessionID,
			request.ExpectedRevision,
		)
	}
	return dependencies.Runtime.DeleteSession(
		r.Context(),
		request.SessionID,
		request.ExpectedRevision,
	)
}

func (s *Server) sessionHistory(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string          `json:"session_id"`
		Since     protocol.Cursor `json:"since_sequence,omitempty"`
		Before    protocol.Cursor `json:"before_sequence,omitempty"`
		Limit     int             `json:"limit,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.Limit == 0 {
		request.Limit = 256
	}
	return dependencies.Runtime.History(r.Context(), app.SessionHistoryQuery{
		SessionID: request.SessionID, Since: request.Since,
		Before: request.Before, Limit: request.Limit,
	})
}

func (s *Server) sessionSnapshot(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.HistoryService.Snapshot(
		r.Context(),
		request.SessionID,
	)
}

func (s *Server) sessionExport(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.HistoryService.Export(
		r.Context(),
		request.SessionID,
	)
}

func (s *Server) sessionMerge(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
		Action    string `json:"action"`
		PlanID    string `json:"plan_id,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	summary, err := dependencies.Runtime.SessionStatus(r.Context(), request.SessionID)
	if err != nil {
		return nil, err
	}
	if summary.Isolation != app.SessionIsolationWorktree ||
		dependencies.SessionWorkspaces == nil {
		return nil, protocol.NewProblem(
			protocol.CodeConflict,
			"session has no isolated Chat worktree",
			false,
			nil,
		)
	}
	switch summary.Status {
	case protocol.SessionStatusRunning,
		protocol.SessionStatusAwaitingApproval,
		protocol.SessionStatusAwaitingInput:
		return nil, protocol.NewProblem(
			protocol.CodeConflict,
			"cannot merge while the Chat turn is active",
			true,
			nil,
		)
	}
	var plan any
	switch request.Action {
	case "preview":
		if request.PlanID != "" {
			return nil, protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"preview does not accept plan_id",
				false,
				nil,
			)
		}
		plan, err = dependencies.SessionWorkspaces.PlanMerge(
			r.Context(),
			request.SessionID,
			summary.ThreadID,
		)
	case "apply":
		if len(request.PlanID) != 64 {
			return nil, protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"apply requires a valid plan_id",
				false,
				nil,
			)
		}
		plan, err = dependencies.SessionWorkspaces.ApplyMerge(
			r.Context(),
			request.SessionID,
			summary.ThreadID,
			request.PlanID,
		)
	default:
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"action must be preview or apply",
			false,
			nil,
		)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"session_id": request.SessionID,
		"thread_id":  summary.ThreadID,
		"action":     request.Action,
		"plan":       plan,
	}, nil
}

func (s *Server) operationSubmit(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID      string                 `json:"session_id"`
		Kind           protocol.OperationKind `json:"kind"`
		Payload        json.RawMessage        `json:"payload"`
		IdempotencyKey string                 `json:"idempotency_key"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if !webOperationExposed(request.Kind) {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			fmt.Sprintf("operation %q is not exposed by the Web Host", request.Kind),
			false,
			nil,
		)
	}
	payload, err := protocol.DecodeOperationPayload(request.Kind, request.Payload)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			false,
			err,
		)
	}
	if err := validateWebOperationPayload(payload); err != nil {
		return nil, err
	}
	if err := validateWebEditorContext(
		r.Context(),
		dependencies,
		payload,
	); err != nil {
		return nil, err
	}
	return dependencies.Runtime.SubmitForSession(
		r.Context(),
		app.SubmitSessionOperation{
			SessionID: request.SessionID, Kind: request.Kind,
			Payload: payload, IdempotencyKey: request.IdempotencyKey,
			WorkspaceIdentity: &dependencies.WorkspaceIdentity,
		},
	)
}

func validateWebOperationPayload(payload protocol.OperationPayload) error {
	start, ok := payload.(*protocol.StartTurnPayload)
	if !ok || start.PlanExecution == nil && start.Recovery == nil {
		return nil
	}
	return protocol.NewProblem(
		protocol.CodeInvalidArgument,
		"Plan execution and Turn recovery require their dedicated Web routes",
		false,
		nil,
	)
}

func (s *Server) profileGet(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.SessionProfile(r.Context(), request.SessionID)
}

func (s *Server) profileUpdate(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID        string                       `json:"session_id"`
		ThreadID         protocol.ThreadID            `json:"thread_id"`
		ExpectedRevision uint64                       `json:"expected_revision"`
		Patch            protocol.SessionProfilePatch `json:"patch"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.UpdateSessionProfile(
		r.Context(),
		request.SessionID,
		request.ThreadID,
		request.ExpectedRevision,
		request.Patch,
	)
}

func (s *Server) agentPresetList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request protocol.AgentPresetListRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.AgentPresetService.List(r.Context(), request)
}

func (s *Server) agentPresetSave(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request protocol.AgentPresetSaveRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.AgentPresetService.Save(r.Context(), request)
}

func (s *Server) agentPresetDelete(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request protocol.AgentPresetDeleteRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.AgentPresetService.Delete(r.Context(), request)
}

func (s *Server) agentPresetApply(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request protocol.AgentPresetApplyRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.AgentPresetService.Apply(r.Context(), request)
}

func (s *Server) toolCatalog(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.SessionToolCatalog(
		r.Context(),
		request.SessionID,
	)
}

type checkpointRequest struct {
	SessionID    string `json:"session_id"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Title        string `json:"title,omitempty"`
}

func (s *Server) checkpointList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request checkpointRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.Checkpoints(
		r.Context(),
		request.SessionID,
		request.Limit,
	)
}

func (s *Server) checkpointGet(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request checkpointRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.CheckpointID == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"checkpoint_id is required",
			false,
			nil,
		)
	}
	return dependencies.Runtime.Checkpoint(
		r.Context(),
		request.SessionID,
		request.CheckpointID,
	)
}

func (s *Server) checkpointRestore(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request checkpointRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.CheckpointID == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"checkpoint_id is required",
			false,
			nil,
		)
	}
	return dependencies.Runtime.RestoreCheckpoint(
		r.Context(),
		request.SessionID,
		request.CheckpointID,
	)
}

func (s *Server) checkpointFork(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request checkpointRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.CheckpointID == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"checkpoint_id is required",
			false,
			nil,
		)
	}
	return dependencies.Runtime.ForkCheckpoint(
		r.Context(),
		request.SessionID,
		request.CheckpointID,
		request.Title,
	)
}

func (s *Server) turnRecover(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request protocol.TurnRecoveryRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	prepared, err := dependencies.Runtime.PrepareTurnRecovery(r.Context(), request)
	if err != nil {
		return nil, err
	}
	return dependencies.Runtime.SubmitForSession(
		r.Context(),
		app.SubmitSessionOperation{
			SessionID:         request.SessionID,
			Kind:              protocol.OperationStartTurn,
			IdempotencyKey:    prepared.IdempotencyKey,
			WorkspaceIdentity: &dependencies.WorkspaceIdentity,
			Payload: &protocol.StartTurnPayload{
				Prompt:        prepared.Prompt,
				DisplayPrompt: prepared.DisplayPrompt,
				Intent:        prepared.Intent,
				Recovery:      &prepared.Recovery,
			},
		},
	)
}

func (s *Server) turnWithdraw(r *http.Request, dependencies Dependencies) (any, error) {
	var request protocol.TurnWithdrawRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if err := dependencies.Runtime.WithdrawTurn(r.Context(), request); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Server) turnQueue(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.TurnQueueService.List(
		r.Context(),
		request.SessionID,
	)
}

func (s *Server) planGet(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.SessionPlan(r.Context(), request.SessionID)
}

func (s *Server) agentList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID     string `json:"session_id"`
		ParentID      string `json:"parent_id,omitempty"`
		IncludeClosed bool   `json:"include_closed,omitempty"`
		Limit         int    `json:"limit,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if _, err := dependencies.Runtime.SessionStatus(
		r.Context(),
		request.SessionID,
	); err != nil {
		return nil, err
	}
	limit, err := boundedLimit(request.Limit, 100)
	if err != nil {
		return nil, err
	}
	result := make([]runtimeview.Agent, 0)
	if dependencies.Agents != nil {
		values := dependencies.Agents.List(subagent.ListFilter{
			SessionID:     request.SessionID,
			ParentID:      request.ParentID,
			IncludeClosed: request.IncludeClosed,
		})
		if len(values) > limit {
			values = values[:limit]
		}
		for _, value := range values {
			result = append(result, runtimeview.AgentFrom(value))
		}
	}
	return map[string]any{"agents": result}, nil
}

func (s *Server) usageQuery(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Usage == nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			"usage read model is unavailable",
			false,
			nil,
		)
	}
	var request struct {
		SessionID       string            `json:"session_id,omitempty"`
		ThreadID        protocol.ThreadID `json:"thread_id,omitempty"`
		TurnID          protocol.TurnID   `json:"turn_id,omitempty"`
		Provider        string            `json:"provider,omitempty"`
		Model           string            `json:"model,omitempty"`
		Start           time.Time         `json:"start,omitempty"`
		End             time.Time         `json:"end,omitempty"`
		Limit           int               `json:"limit,omitempty"`
		IncludeChildren bool              `json:"include_children,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	limit, err := boundedLimit(request.Limit, 100)
	if err != nil {
		return nil, err
	}
	filter := usagestate.Query{
		SessionID:       request.SessionID,
		ThreadID:        request.ThreadID,
		TurnID:          request.TurnID,
		IncludeChildren: request.IncludeChildren,
		Provider:        request.Provider,
		Model:           request.Model,
		WorkspaceRoot:   dependencies.WorkspaceRoot,
		Start:           request.Start,
		End:             request.End,
		Limit:           limit,
	}
	values, err := dependencies.Usage.QueryAggregates(r.Context(), filter)
	if err != nil {
		return nil, err
	}
	rollup, err := dependencies.Usage.QueryRollup(r.Context(), filter)
	if err != nil {
		return nil, err
	}
	result := make([]runtimeview.Usage, 0, len(values))
	for _, value := range values {
		result = append(result, runtimeview.UsageFrom(value))
	}
	return map[string]any{
		"usage":  result,
		"rollup": runtimeview.UsageRollupFrom(rollup),
	}, nil
}

func (s *Server) traceQuery(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request tracestate.TraceQuery
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if len(request.TurnIDs) > s.capacity.MaxTraceTurns {
		return nil, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"trace query exceeds the Web API turn limit",
			false,
			nil,
		)
	}
	if _, err := dependencies.Runtime.SessionStatus(
		r.Context(),
		request.SessionID,
	); err != nil {
		return nil, err
	}
	if dependencies.Runtime.TraceQuery == nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			"trace query is unavailable",
			false,
			nil,
		)
	}
	return dependencies.Runtime.TraceQuery.Query(r.Context(), request)
}

func boundedLimit(value, fallback int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 1 || value > 1000 {
		return 0, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"limit must be between 1 and 1000",
			false,
			nil,
		)
	}
	return value, nil
}

func (s *Server) extensionList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Extensions == nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			"extension control is unavailable",
			false,
			nil,
		)
	}
	var request struct {
		Kind protocol.ExtensionControlKind `json:"kind,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	if request.Kind == "" {
		request.Kind = protocol.ExtensionControlAll
	}
	return dependencies.Extensions.Snapshot(r.Context(), request.Kind)
}

func (s *Server) extensionControl(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Extensions == nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			"extension control is unavailable",
			false,
			nil,
		)
	}
	var operation protocol.ExtensionControlOperation
	if err := s.decodeRequest(r, &operation); err != nil {
		return nil, err
	}
	return dependencies.Extensions.Submit(r.Context(), operation)
}

func (s *Server) workspaceBrowse(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace query is unavailable")
	}
	var request struct {
		Path  string `json:"path,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := dependencies.Workspace.Browse(
		r.Context(),
		request.Path,
		request.Limit,
	)
	return result, workspaceQueryError(err)
}

func (s *Server) workspaceSearch(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace query is unavailable")
	}
	var request struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := dependencies.Workspace.Search(
		r.Context(),
		request.Query,
		request.Limit,
	)
	return result, workspaceQueryError(err)
}

func (s *Server) workspaceResource(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace query is unavailable")
	}
	var request struct {
		Path string `json:"path"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := dependencies.Workspace.Resource(r.Context(), request.Path)
	if err != nil {
		return nil, workspaceQueryError(err)
	}
	resourceURI, err := workspaceResourceURI(
		dependencies.WorkspaceIdentity,
		result.Path,
	)
	if err != nil {
		return nil, err
	}
	contentHandle, err := s.issueContentHandle(
		dependencies.WorkspaceIdentity,
		result.Path,
		result.Digest,
	)
	if err != nil {
		return nil, err
	}
	return struct {
		workspacequery.Resource
		URI             string `json:"uri"`
		DocumentVersion int    `json:"document_version"`
		ContentHandle   string `json:"content_handle"`
	}{
		Resource:        result,
		URI:             resourceURI,
		DocumentVersion: 1,
		ContentHandle:   contentHandle,
	}, nil
}

func (s *Server) workspaceImage(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace query is unavailable")
	}
	var request struct {
		Path string `json:"path"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	image, err := dependencies.Workspace.Image(r.Context(), request.Path)
	if err != nil {
		return nil, workspaceQueryError(err)
	}
	resourceURI, err := workspaceResourceURI(
		dependencies.WorkspaceIdentity,
		image.Path,
	)
	if err != nil {
		return nil, err
	}
	contentHandle, err := s.issueContentHandle(
		dependencies.WorkspaceIdentity,
		image.Path,
		image.Digest,
		image.MediaType,
	)
	if err != nil {
		return nil, err
	}
	return struct {
		workspacequery.ImageResource
		URI             string `json:"uri"`
		DocumentVersion int    `json:"document_version"`
		Label           string `json:"label"`
		ContentHandle   string `json:"content_handle"`
	}{
		ImageResource:   image,
		URI:             resourceURI,
		DocumentVersion: 1,
		Label:           path.Base(image.Path),
		ContentHandle:   contentHandle,
	}, nil
}

type workspaceSymbol struct {
	Path            string               `json:"path"`
	Name            string               `json:"name"`
	Kind            string               `json:"kind"`
	Container       string               `json:"container,omitempty"`
	Line            int                  `json:"line"`
	URI             string               `json:"uri"`
	DocumentVersion int                  `json:"document_version"`
	Digest          string               `json:"digest"`
	Range           protocol.EditorRange `json:"range"`
	SelectionRange  protocol.EditorRange `json:"selection_range"`
}

func (s *Server) workspaceSymbols(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.RepositoryIndex == nil || dependencies.Workspace == nil {
		return nil, unavailable("workspace symbol index is unavailable")
	}
	var request struct {
		Query string `json:"query"`
		Path  string `json:"path,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"workspace symbol query is required",
			false,
			nil,
		)
	}
	limit, err := boundedLimit(request.Limit, 100)
	if err != nil {
		return nil, err
	}
	query := repoindex.Query{Name: request.Query, Limit: limit}
	if request.Path != "" {
		query.Paths = []string{request.Path}
	}
	found, snapshot, err := dependencies.RepositoryIndex.Symbols(r.Context(), query)
	if err != nil {
		return nil, err
	}
	result := struct {
		Query   string            `json:"query"`
		Status  string            `json:"status"`
		Detail  string            `json:"detail,omitempty"`
		Symbols []workspaceSymbol `json:"symbols"`
	}{
		Query: request.Query, Status: snapshot.Status, Detail: snapshot.Detail,
		Symbols: make([]workspaceSymbol, 0, len(found)),
	}
	for _, symbol := range found {
		value, err := resolveWorkspaceSymbol(
			r.Context(),
			dependencies.Workspace,
			dependencies.WorkspaceIdentity,
			symbol,
		)
		if err != nil {
			return nil, err
		}
		result.Symbols = append(result.Symbols, value)
	}
	return result, nil
}

func resolveWorkspaceSymbol(
	ctx context.Context,
	workspace *workspacequery.Service,
	identity protocol.WorkspaceIdentity,
	symbol repoindex.Symbol,
) (workspaceSymbol, error) {
	resource, err := workspace.Resource(ctx, symbol.Path)
	if err != nil {
		return workspaceSymbol{}, workspaceQueryError(err)
	}
	resourceURI, err := workspaceResourceURI(identity, resource.Path)
	if err != nil {
		return workspaceSymbol{}, err
	}
	lines := strings.Split(resource.Content, "\n")
	lineIndex := symbol.Line - 1
	if lineIndex < 0 || lineIndex >= len(lines) {
		return workspaceSymbol{}, protocol.NewProblem(
			protocol.CodeConflict,
			"workspace symbol index is stale",
			true,
			nil,
		)
	}
	line := strings.TrimSuffix(lines[lineIndex], "\r")
	byteStart := strings.Index(line, symbol.Name)
	if byteStart < 0 {
		return workspaceSymbol{}, protocol.NewProblem(
			protocol.CodeConflict,
			"workspace symbol index is stale",
			true,
			nil,
		)
	}
	start := utf16Length(line[:byteStart])
	end := start + utf16Length(symbol.Name)
	return workspaceSymbol{
		Path: symbol.Path, Name: symbol.Name, Kind: symbol.Kind,
		Container: symbol.Container, Line: symbol.Line,
		URI: resourceURI, DocumentVersion: 1, Digest: resource.Digest,
		Range: protocol.EditorRange{
			Start: protocol.EditorPosition{Line: lineIndex, Character: 0},
			End: protocol.EditorPosition{
				Line: lineIndex, Character: utf16Length(line),
			},
		},
		SelectionRange: protocol.EditorRange{
			Start: protocol.EditorPosition{Line: lineIndex, Character: start},
			End:   protocol.EditorPosition{Line: lineIndex, Character: end},
		},
	}, nil
}

func utf16Length(value string) int {
	length := 0
	for _, character := range value {
		length++
		if character > 0xffff {
			length++
		}
	}
	return length
}

type workspaceDiagnosticContext struct {
	CallID  string                          `json:"call_id"`
	Tool    string                          `json:"tool"`
	Status  string                          `json:"status"`
	Message string                          `json:"message,omitempty"`
	Context protocol.EditorContextReference `json:"context"`
}

func (s *Server) workspaceDiagnostics(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace query is unavailable")
	}
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	summary, err := dependencies.Runtime.SessionStatus(r.Context(), request.SessionID)
	if err != nil {
		return nil, err
	}
	values, err := collectWorkspaceDiagnostics(
		r.Context(),
		dependencies,
		summary.ThreadID,
		100,
	)
	if err != nil {
		return nil, err
	}
	return struct {
		SessionID   string                       `json:"session_id"`
		ThreadID    protocol.ThreadID            `json:"thread_id"`
		Diagnostics []workspaceDiagnosticContext `json:"diagnostics"`
	}{
		SessionID: request.SessionID, ThreadID: summary.ThreadID,
		Diagnostics: values,
	}, nil
}

func collectWorkspaceDiagnostics(
	ctx context.Context,
	dependencies Dependencies,
	threadID protocol.ThreadID,
	limit int,
) ([]workspaceDiagnosticContext, error) {
	values := make([]workspaceDiagnosticContext, 0)
	cursor := protocol.Cursor(0)
	for {
		events, more, err := dependencies.Runtime.ReplayEvents(ctx, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			cursor = event.Sequence
			if event.ThreadID != threadID {
				continue
			}
			diagnostics, ok := event.Data.(*protocol.DiagnosticsData)
			if !ok {
				continue
			}
			for _, receipt := range diagnostics.Receipts {
				contextReference, ok, err := workspaceDiagnosticReference(
					ctx,
					dependencies.Workspace,
					dependencies.WorkspaceIdentity,
					receipt,
				)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				values = append(values, workspaceDiagnosticContext{
					CallID: diagnostics.CallID, Tool: diagnostics.Tool,
					Status: receipt.Status, Message: receipt.Message,
					Context: contextReference,
				})
				if len(values) > limit {
					values = values[len(values)-limit:]
				}
			}
		}
		if !more || len(events) == 0 {
			return values, nil
		}
	}
}

func workspaceDiagnosticReference(
	ctx context.Context,
	workspace *workspacequery.Service,
	identity protocol.WorkspaceIdentity,
	receipt protocol.DiagnosticReceipt,
) (protocol.EditorContextReference, bool, error) {
	resource, err := workspace.Resource(ctx, receipt.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return protocol.EditorContextReference{}, false, nil
		}
		return protocol.EditorContextReference{}, false, workspaceQueryError(err)
	}
	resourceURI, err := workspaceResourceURI(identity, resource.Path)
	if err != nil {
		return protocol.EditorContextReference{}, false, err
	}
	values := make([]protocol.EditorDiagnostic, 0, min(len(receipt.Diagnostics), 32))
	omitted := 0
	for _, diagnostic := range receipt.Diagnostics {
		diagnosticPath := diagnostic.Path
		if diagnosticPath == "" {
			diagnosticPath = receipt.Path
		}
		if diagnosticPath != receipt.Path ||
			!validWebDiagnostic(diagnostic, resource.Content) {
			continue
		}
		if len(values) == cap(values) {
			omitted++
			continue
		}
		values = append(values, protocol.EditorDiagnostic{
			Range: protocol.EditorRange{
				Start: protocol.EditorPosition{
					Line:      diagnostic.Range.Start.Line,
					Character: diagnostic.Range.Start.Character,
				},
				End: protocol.EditorPosition{
					Line:      diagnostic.Range.End.Line,
					Character: diagnostic.Range.End.Character,
				},
			},
			Severity: diagnostic.Severity, Code: diagnostic.Code,
			Message: diagnostic.Message, Source: diagnostic.Source,
		})
	}
	if len(values) == 0 {
		return protocol.EditorContextReference{}, false, nil
	}
	return protocol.EditorContextReference{
		Kind:   protocol.EditorContextDiagnostics,
		Source: protocol.EditorContextSourceCodeAction,
		URI:    resourceURI, Path: resource.Path, DocumentVersion: 1,
		Digest: resource.Digest, Diagnostics: values,
		OmittedDiagnostics: omitted, Explicit: true,
	}, true, nil
}

func validWebDiagnostic(value protocol.Diagnostic, content string) bool {
	if value.Message == "" ||
		value.Range.Start.Line < 0 || value.Range.Start.Character < 0 ||
		value.Range.End.Line < value.Range.Start.Line ||
		(value.Range.End.Line == value.Range.Start.Line &&
			value.Range.End.Character < value.Range.Start.Character) {
		return false
	}
	switch value.Severity {
	case "error", "warning", "information", "hint":
		return webRangeWithinContent(value.Range, content)
	default:
		return false
	}
}

func webRangeWithinContent(value protocol.DiagnosticRange, content string) bool {
	lines := strings.Split(content, "\n")
	if value.Start.Line >= len(lines) || value.End.Line >= len(lines) {
		return false
	}
	startLine := strings.TrimSuffix(lines[value.Start.Line], "\r")
	endLine := strings.TrimSuffix(lines[value.End.Line], "\r")
	return value.Start.Character <= utf16Length(startLine) &&
		value.End.Character <= utf16Length(endLine)
}

func validateWebEditorContext(
	ctx context.Context,
	dependencies Dependencies,
	payload protocol.OperationPayload,
) error {
	var (
		threadID   protocol.ThreadID
		references []protocol.EditorContextReference
	)
	switch value := payload.(type) {
	case *protocol.StartTurnPayload:
		threadID, references = value.ThreadID, value.Context
	case *protocol.EnqueueTurnPayload:
		threadID, references = value.ThreadID, value.Context
	default:
		return nil
	}
	if len(references) == 0 {
		return nil
	}
	terminalReferences := make([]protocol.EditorContextReference, 0)
	diagnosticReferences := make([]protocol.EditorContextReference, 0)
	for _, reference := range references {
		switch reference.Kind {
		case protocol.EditorContextFile,
			protocol.EditorContextSelection,
			protocol.EditorContextSymbol,
			protocol.EditorContextDiagnostics:
			if dependencies.Workspace == nil {
				return unavailable("workspace query is unavailable")
			}
			resource, err := dependencies.Workspace.Resource(ctx, reference.Path)
			if err != nil {
				return workspaceQueryError(err)
			}
			resourceURI, err := workspaceResourceURI(
				dependencies.WorkspaceIdentity,
				resource.Path,
			)
			if err != nil {
				return err
			}
			if reference.Path != resource.Path ||
				reference.URI != resourceURI ||
				reference.DocumentVersion != 1 ||
				reference.Digest != resource.Digest {
				return protocol.NewProblem(
					protocol.CodeConflict,
					"workspace context resource is stale or was not server-issued",
					true,
					nil,
				)
			}
			if reference.Kind == protocol.EditorContextSymbol {
				if err := validateWebSymbolContext(
					ctx,
					dependencies,
					reference,
				); err != nil {
					return err
				}
			}
			if reference.Kind == protocol.EditorContextDiagnostics {
				diagnosticReferences = append(diagnosticReferences, reference)
			}
		case protocol.EditorContextImage:
			if reference.Path == "" {
				data, err := base64.StdEncoding.DecodeString(reference.Content)
				if err != nil || len(data) == 0 || len(data) > 5<<20 {
					return protocol.NewProblem(
						protocol.CodeInvalidArgument,
						"inline image attachment is invalid",
						false,
						err,
					)
				}
				digest := sha256.Sum256(data)
				detected := http.DetectContentType(data)
				if reference.Digest != hex.EncodeToString(digest[:]) ||
					reference.MediaType != detected ||
					!supportedImageMediaType(detected) {
					return protocol.NewProblem(
						protocol.CodeConflict,
						"inline image attachment failed content validation",
						false,
						nil,
					)
				}
				continue
			}
			if dependencies.Workspace == nil {
				return unavailable("workspace query is unavailable")
			}
			image, err := dependencies.Workspace.Image(ctx, reference.Path)
			if err != nil {
				return workspaceQueryError(err)
			}
			resourceURI, err := workspaceResourceURI(
				dependencies.WorkspaceIdentity,
				image.Path,
			)
			if err != nil {
				return err
			}
			if reference.Path != image.Path ||
				reference.URI != resourceURI ||
				reference.DocumentVersion != 1 ||
				reference.Digest != image.Digest ||
				reference.MediaType != image.MediaType ||
				reference.Label != path.Base(image.Path) {
				return protocol.NewProblem(
					protocol.CodeConflict,
					"workspace image context is stale or was not server-issued",
					true,
					nil,
				)
			}
		case protocol.EditorContextAttachment:
			digest := sha256.Sum256([]byte(reference.Content))
			if reference.Digest != hex.EncodeToString(digest[:]) {
				return protocol.NewProblem(
					protocol.CodeConflict,
					"text attachment digest does not match content",
					false,
					nil,
				)
			}
		case protocol.EditorContextGitDiff:
			diff := dependencies.Runtime.FormatTurnDiff(threadID)
			digest := sha256.Sum256([]byte(diff))
			if reference.Content != diff ||
				reference.Digest != hex.EncodeToString(digest[:]) {
				return protocol.NewProblem(
					protocol.CodeConflict,
					"workspace diff context is stale or was not server-issued",
					true,
					nil,
				)
			}
		case protocol.EditorContextTerminal:
			terminalReferences = append(terminalReferences, reference)
		default:
			continue
		}
	}
	if err := validateWebTerminalContexts(
		ctx,
		dependencies.Runtime,
		threadID,
		terminalReferences,
	); err != nil {
		return err
	}
	return validateWebDiagnosticContexts(
		ctx,
		dependencies,
		threadID,
		diagnosticReferences,
	)
}

func validateWebSymbolContext(
	ctx context.Context,
	dependencies Dependencies,
	reference protocol.EditorContextReference,
) error {
	if dependencies.RepositoryIndex == nil || reference.Symbol == nil ||
		reference.Symbol.SelectionRange == nil || reference.Range == nil {
		return unavailable("workspace symbol index is unavailable")
	}
	found, snapshot, err := dependencies.RepositoryIndex.Symbols(
		ctx,
		repoindex.Query{
			Name: reference.Symbol.Name, Exact: true,
			Paths: []string{reference.Path}, Limit: 100,
		},
	)
	if err != nil {
		return err
	}
	if !snapshot.Ready() {
		return unavailable("workspace symbol index is unavailable")
	}
	for _, symbol := range found {
		if symbol.Kind != reference.Symbol.Kind {
			continue
		}
		resolved, err := resolveWorkspaceSymbol(
			ctx,
			dependencies.Workspace,
			dependencies.WorkspaceIdentity,
			symbol,
		)
		if err != nil {
			return err
		}
		if resolved.Digest == reference.Digest &&
			resolved.Range == *reference.Range &&
			resolved.SelectionRange == *reference.Symbol.SelectionRange {
			return nil
		}
	}
	return protocol.NewProblem(
		protocol.CodeConflict,
		"workspace symbol context is stale or was not server-issued",
		true,
		nil,
	)
}

func validateWebTerminalContexts(
	ctx context.Context,
	runtime *app.Runtime,
	threadID protocol.ThreadID,
	references []protocol.EditorContextReference,
) error {
	if len(references) == 0 {
		return nil
	}
	if runtime == nil {
		return unavailable("runtime is unavailable")
	}
	pending := make(map[string]protocol.EditorContextReference, len(references))
	for _, reference := range references {
		pending[reference.Label+"\x00"+reference.Digest] = reference
	}
	cursor := protocol.Cursor(0)
	for len(pending) > 0 {
		events, more, err := runtime.ReplayEvents(ctx, cursor, 1000)
		if err != nil {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"terminal context source is no longer available",
				true,
				err,
			)
		}
		for _, event := range events {
			cursor = event.Sequence
			if event.ThreadID != threadID {
				continue
			}
			result, ok := event.Data.(*protocol.ToolResultData)
			if !ok {
				continue
			}
			digest := sha256.Sum256([]byte(result.Output))
			key := result.CallID + "\x00" + hex.EncodeToString(digest[:])
			reference, wanted := pending[key]
			if wanted && reference.Content == result.Output {
				delete(pending, key)
			}
		}
		if !more || len(events) == 0 {
			break
		}
	}
	if len(pending) != 0 {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"terminal context is stale or was not issued for this thread",
			true,
			nil,
		)
	}
	return nil
}

func validateWebDiagnosticContexts(
	ctx context.Context,
	dependencies Dependencies,
	threadID protocol.ThreadID,
	references []protocol.EditorContextReference,
) error {
	if len(references) == 0 {
		return nil
	}
	pending := make(map[string]struct{}, len(references))
	for _, reference := range references {
		pending[webEditorContextKey(reference)] = struct{}{}
	}
	values, err := collectWorkspaceDiagnostics(
		ctx,
		dependencies,
		threadID,
		1000,
	)
	if err != nil {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"diagnostic context source is no longer available",
			true,
			err,
		)
	}
	for _, value := range values {
		delete(pending, webEditorContextKey(value.Context))
	}
	if len(pending) != 0 {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"diagnostic context is stale or was not issued for this thread",
			true,
			nil,
		)
	}
	return nil
}

func webEditorContextKey(reference protocol.EditorContextReference) string {
	encoded, _ := json.Marshal(reference)
	return string(encoded)
}

func workspaceResourceURI(
	identity protocol.WorkspaceIdentity,
	resourcePath string,
) (string, error) {
	resourceURI, err := url.Parse(identity.EditorURI)
	if err != nil {
		return "", err
	}
	resourceURI.Path = path.Join(resourceURI.Path, resourcePath)
	resourceURI.RawPath = ""
	return resourceURI.String(), nil
}

func (s *Server) workspaceDiff(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		SessionID string `json:"session_id"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	summary, err := dependencies.Runtime.SessionStatus(
		r.Context(),
		request.SessionID,
	)
	if err != nil {
		return nil, err
	}
	diff := dependencies.Runtime.FormatTurnDiff(summary.ThreadID)
	digest := sha256.Sum256([]byte(diff))
	return map[string]any{
		"session_id": request.SessionID,
		"thread_id":  summary.ThreadID,
		"diff":       diff,
		"digest":     hex.EncodeToString(digest[:]),
	}, nil
}

func (s *Server) credentialStatus(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if dependencies.Credentials == nil {
		return nil, unavailable("credential management is unavailable")
	}
	return dependencies.Credentials.Status(r.Context())
}

func (s *Server) credentialSetKeyring(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Credentials == nil {
		return nil, unavailable("credential management is unavailable")
	}
	var request struct {
		Secret string `json:"secret"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Credentials.SetKeyring(r.Context(), request.Secret)
}

func (s *Server) credentialClearKeyring(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if dependencies.Credentials == nil {
		return nil, unavailable("credential management is unavailable")
	}
	return dependencies.Credentials.ClearKeyring(r.Context())
}

func (s *Server) credentialValidate(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if dependencies.Credentials == nil {
		return nil, unavailable("credential management is unavailable")
	}
	return dependencies.Credentials.Validate(r.Context())
}

func workspaceQueryError(err error) error {
	if err == nil {
		return nil
	}
	return protocol.NewProblem(
		protocol.CodeInvalidArgument,
		err.Error(),
		false,
		err,
	)
}

func unavailable(message string) error {
	return protocol.NewProblem(
		protocol.CodeUnavailable,
		message,
		false,
		nil,
	)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.ready.Load() || s.draining.Load() {
		writeProblem(w, r, http.StatusServiceUnavailable, protocol.NewProblem(
			protocol.CodeUnavailable,
			"Runtime is not ready",
			true,
			nil,
		))
		return
	}
	if !s.acquireConnection() {
		writeProblem(w, r, http.StatusTooManyRequests, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"WebSocket connection capacity is exhausted",
			true,
			nil,
		))
		return
	}
	defer s.connections.Add(-1)
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	connection.SetReadLimit(s.capacity.MaxWebSocketFrameBytes)
	authContext, cancel := context.WithTimeout(r.Context(), websocketTimeout)
	_, data, err := connection.Read(authContext)
	cancel()
	if err != nil {
		_ = connection.Close(websocket.StatusPolicyViolation, "authentication required")
		return
	}
	var auth authFrame
	if decodeStrict(data, &auth) != nil ||
		auth.Type != "authenticate" ||
		!tokenEqual(auth.Token, s.token) {
		_ = connection.Close(websocket.StatusPolicyViolation, "authentication failed")
		return
	}
	dependencies, _, found := s.workspaceSnapshot(auth.WorkspaceID)
	if !found {
		_ = connection.Close(
			websocket.StatusPolicyViolation,
			"workspace is not registered",
		)
		return
	}
	events, err := dependencies.Runtime.EventsLimited(
		r.Context(),
		auth.Cursor,
		s.capacity.MaxReplayEvents,
	)
	if err != nil {
		problem := problemFor(err)
		frameType := "resync"
		closeStatus := websocket.StatusTryAgainLater
		closeReason := "event replay unavailable"
		var gap *app.CursorGapError
		if errors.As(err, &gap) {
			frameType = "desync"
			closeStatus = websocket.StatusPolicyViolation
			closeReason = "cursor history expired"
		}
		_ = writeSocket(r.Context(), connection, eventFrame{
			Type: frameType, ProtocolVersion: webProtocol, Problem: problem,
		})
		_ = connection.Close(closeStatus, closeReason)
		return
	}
	if err := writeSocket(r.Context(), connection, eventFrame{
		Type: "hello", ProtocolVersion: webProtocol,
		Sequence: auth.Cursor,
	}); err != nil {
		return
	}
	clientInput := make(chan error, 1)
	go func() {
		_, _, err := connection.Read(r.Context())
		clientInput <- err
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case err := <-clientInput:
			if err == nil {
				_ = connection.Close(
					websocket.StatusPolicyViolation,
					"event stream is downlink-only",
				)
			}
			return
		case event, open := <-events:
			if !open {
				_ = connection.Close(websocket.StatusServiceRestart, "event stream closed")
				return
			}
			sessionID, authorized := authorizedEventSession(
				r.Context(),
				dependencies.Runtime,
				event,
			)
			if !authorized {
				if err := writeSocket(r.Context(), connection, eventFrame{
					Type: "watermark", ProtocolVersion: webProtocol,
					Sequence: event.Sequence,
				}); err != nil {
					return
				}
				continue
			}
			value := event
			if err := writeSocket(r.Context(), connection, eventFrame{
				Type: "event", ProtocolVersion: webProtocol,
				SessionID: sessionID, Sequence: event.Sequence, Event: &value,
			}); err != nil {
				return
			}
		}
	}
}

func authorizedEventSession(
	ctx context.Context,
	runtime *app.Runtime,
	event protocol.Event,
) (string, bool) {
	if runtime == nil {
		return "", false
	}
	sessionID := protocol.EventSessionID(event.Data)
	var err error
	if sessionID == "" {
		if event.ThreadID == "" {
			return "", false
		}
		sessionID, err = runtime.SessionForThread(ctx, event.ThreadID)
	}
	if err != nil || strings.TrimSpace(sessionID) == "" {
		return "", false
	}
	if _, err := runtime.SessionStatus(ctx, sessionID); err != nil {
		return "", false
	}
	return sessionID, true
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if hasTraversalSegment(r.URL.EscapedPath()) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	if strings.HasSuffix(name, ".br") || strings.HasSuffix(name, ".gz") {
		http.NotFound(w, r)
		return
	}
	originalName := name
	data, err := fs.ReadFile(s.assets, originalName)
	if err != nil {
		data = s.index
		originalName = "index.html"
	} else if path.Ext(originalName) == ".js" || path.Ext(originalName) == ".css" {
		w.Header().Set("Vary", "Accept-Encoding")
		for _, candidate := range []struct {
			encoding string
			suffix   string
		}{
			{encoding: "br", suffix: ".br"},
			{encoding: "gzip", suffix: ".gz"},
		} {
			if !acceptsEncoding(r.Header.Get("Accept-Encoding"), candidate.encoding) {
				continue
			}
			compressed, readErr := fs.ReadFile(s.assets, originalName+candidate.suffix)
			if readErr == nil {
				data = compressed
				w.Header().Set("Content-Encoding", candidate.encoding)
				break
			}
		}
	}
	digest := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	switch path.Ext(originalName) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if originalName == "theme-bootstrap.js" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

func acceptsEncoding(header, target string) bool {
	explicit := false
	explicitQuality := 0.0
	wildcardQuality := 0.0
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		encoding := strings.TrimSpace(strings.ToLower(parts[0]))
		if encoding != target && encoding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if found && strings.EqualFold(name, "q") {
				parsed, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					quality = 0
					break
				}
				quality = parsed
			}
		}
		if encoding == target {
			explicit = true
			explicitQuality = quality
		} else {
			wildcardQuality = quality
		}
	}
	if explicit {
		return explicitQuality > 0
	}
	return wildcardQuality > 0
}

func (s *Server) browserFence(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.expectedHost {
			http.Error(w, "invalid Host", http.StatusForbidden)
			return
		}
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" &&
			(origin == "null" || origin != s.origin) {
			http.Error(w, "invalid Origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				dependencies, _ := s.snapshot()
				if dependencies.Diagnostics != nil {
					_, _ = fmt.Fprintf(
						dependencies.Diagnostics,
						"qcode: web request panic route=%q\n",
						r.URL.Path,
					)
				}
				writeProblem(w, r, http.StatusInternalServerError, protocol.NewProblem(
					protocol.CodeInternal,
					"Web request failed",
					false,
					nil,
				))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data: blob: https:; connect-src 'self' "+
				strings.Replace(s.origin, "http://", "ws://", 1)+"; "+
				"object-src 'none'; base-uri 'none'; frame-ancestors 'none'; "+
				"form-action 'none'",
		)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	return strings.HasPrefix(value, prefix) &&
		tokenEqual(strings.TrimPrefix(value, prefix), s.token)
}

func (s *Server) snapshot() (Dependencies, *protocol.Problem) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dependencies, s.bootProblem
}

func (s *Server) decodeRequest(r *http.Request, target any) error {
	data, err := io.ReadAll(io.LimitReader(
		r.Body,
		s.capacity.MaxJSONBodyBytes+1,
	))
	if err != nil {
		return protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"read request body: "+err.Error(),
			false,
			err,
		)
	}
	if int64(len(data)) > s.capacity.MaxJSONBodyBytes {
		return protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"request body exceeds the Web API limit",
			false,
			nil,
		)
	}
	if err := decodeStrict(data, target); err != nil {
		return protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"invalid request: "+err.Error(),
			false,
			err,
		)
	}
	return nil
}

func (s *Server) acquireConnection() bool {
	for {
		current := s.connections.Load()
		if current >= int32(s.capacity.MaxConnections) {
			return false
		}
		if s.connections.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func hasTraversalSegment(escapedPath string) bool {
	for _, segment := range strings.Split(escapedPath, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == ".." {
			return true
		}
	}
	return false
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

var webOperationExposure = map[protocol.OperationKind]bool{
	protocol.OperationStartTurn:         true,
	protocol.OperationCancelTurn:        true,
	protocol.OperationSteerTurn:         true,
	protocol.OperationEnqueueTurn:       true,
	protocol.OperationUpdateQueuedTurn:  true,
	protocol.OperationRemoveQueuedTurn:  true,
	protocol.OperationPromoteQueuedTurn: true,
	protocol.OperationApprovalDecision:  true,
	protocol.OperationInputReply:        true,
	protocol.OperationCompactThread:     true,
	protocol.OperationForkThread:        false,
	protocol.OperationRevertTurn:        false,
}

func webOperationExposed(kind protocol.OperationKind) bool {
	return webOperationExposure[kind]
}

func writeSocket(
	ctx context.Context,
	connection *websocket.Conn,
	value any,
) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writeContext, cancel := context.WithTimeout(ctx, websocketTimeout)
	defer cancel()
	return connection.Write(writeContext, websocket.MessageText, data)
}

func writeApplicationError(w http.ResponseWriter, r *http.Request, err error) {
	problem := problemFor(err)
	status := http.StatusInternalServerError
	switch problem.Code {
	case protocol.CodeInvalidArgument:
		status = http.StatusBadRequest
	case protocol.CodeConflict:
		status = http.StatusConflict
	case protocol.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	case protocol.CodeUnavailable:
		status = http.StatusServiceUnavailable
	case protocol.CodeCanceled:
		status = 499
	case protocol.CodeDeadlineExceeded:
		status = http.StatusGatewayTimeout
	}
	writeProblem(w, r, status, problem)
}

func problemFor(err error) *protocol.Problem {
	var problem *protocol.Problem
	if errors.As(err, &problem) {
		copy := *problem
		return &copy
	}
	return protocol.NewProblem(
		protocol.CodeInternal,
		"internal Web API error",
		false,
		err,
	)
}

func writeProblem(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	problem *protocol.Problem,
) {
	writeJSON(w, status, responseEnvelope{
		Version: webProtocol, RequestID: requestID(r), Problem: problem,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func requestID(r *http.Request) string {
	value := r.Header.Get("X-QCode-Request-ID")
	if len(value) > 256 {
		return ""
	}
	return value
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func newToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Web capability token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func tokenEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
