import type {
  AgentPreset,
  AgentPresetApplyResult,
  AgentPresetList,
  AgentPresetMutationResult,
  AgentPresetProfile,
  AgentList,
  AgentSummary,
  Bootstrap,
  CheckpointList,
  CheckpointForkResult,
  CredentialStatus,
  EditPlan,
  EditorContextReference,
  EditorRange,
  Envelope,
  EventFrame,
  ExtensionControlAction,
  ExtensionControlResult,
  ExtensionProjection,
  GitActionRequest,
  GitActionResult,
  GitOverview,
  GitPatch,
  ModelCatalog,
  ModelCatalogEntry,
  ModelMutationRequest,
  ModelTestResult,
  OperationReceipt,
  PresentationSnapshot,
  Problem,
  ProviderCatalog,
  ProviderCatalogEntry,
  QueuedTurn,
  RuntimeEvent,
  SessionBinding,
  SessionDeleteResult,
  SessionHistoryPage,
  SessionLifecycleUpdate,
  SessionList,
  SessionMergeResult,
  SessionPlanArtifact,
  SessionPlanSnapshot,
  SessionProfileSnapshot,
  SessionProfileUpdateResult,
  SessionSummary,
  SetupProbeRequest,
  SetupProbeResult,
  ConnectionListResult,
  SetupRequest,
  SetupResult,
  TraceSnapshot,
  ToolCatalog,
  TurnQueue,
  UsageQueryResult,
  UsageRollup,
  WorkspaceBrowseResult,
  WorkspaceAddResult,
  WorkspaceCatalog,
  WorkspaceConnection,
  WorkspaceDirectoryResult,
  WorkspaceDescriptor,
  WorkspaceDiagnosticContext,
  WorkspaceDiagnostics,
  WorkspaceDiff,
  WorkspaceGitState,
  WorkspaceImage,
  WorkspaceResource,
  WorkspaceSearchResult,
  WorkspaceSymbol,
  WorkspaceSymbolList
} from "../protocol";
import {
  webEventKinds,
  webProtocolVersion,
  type WebRPCRoute
} from "../protocol/web-host.generated";
import {
  IndexedDBBrowserStorage,
  type BrowserProjectionState,
  type BrowserStorage
} from "./storage";
import {
  ConversationProjection,
  emptyConversationSnapshot,
  type ConversationSnapshot
} from "../projection/conversation";
import {projectTurnQueue} from "../projection/turnQueue";
import {FrameNotifier} from "./notifier";

function hydratePlanArtifact(
  artifact: SessionPlanArtifact | undefined
): SessionPlanArtifact | undefined {
  if (!artifact || artifact.document) return artifact;
  const document = JSON.parse(artifact.body) as SessionPlanArtifact["document"];
  if (document?.version !== 1 || !Array.isArray(document.steps)) {
    throw new Error("Plan Artifact must contain a structured Plan Document");
  }
  return {...artifact, document};
}

export type RuntimePhase =
  | "booting"
  | "setup"
  | "ready"
  | "reconnecting"
  | "desynchronized"
  | "failed"
  | "draining";

export interface RuntimeSnapshot {
  phase: RuntimePhase;
  workspaceRoot: string;
  includeArchived: boolean;
  contextResources: readonly EditorContextReference[];
  messageFeedback: Readonly<Record<string, "positive" | "negative">>;
  sessions: readonly SessionSummary[];
  sessionSearchQuery: string;
  sessionSearchResults: readonly SessionSummary[];
  selectedSessionID: string;
  hydratingSessionID: string;
  events: readonly RuntimeEvent[];
  queuedTurns: readonly QueuedTurn[];
  conversation: ConversationSnapshot;
  historyMoreBefore: boolean;
  providers: readonly ProviderCatalogEntry[];
  models: readonly ModelCatalogEntry[];
  workspaces: readonly WorkspaceDescriptor[];
  selectedWorkspaceID: string;
  profile?: SessionProfileSnapshot;
  tools: Readonly<ToolCatalog["tools"]>;
  checkpoints: Readonly<CheckpointList["checkpoints"]>;
  plan?: SessionPlanArtifact;
  agents: readonly AgentSummary[];
  usage?: UsageRollup;
  trace?: TraceSnapshot;
  tracePhase: "idle" | "loading" | "ready" | "unavailable";
  traceProblem?: string;
  extensions: readonly ExtensionProjection[];
  mergePlan?: EditPlan;
  problem?: Problem;
  socketConnected: boolean;
}

type Listener = () => void;
type BufferedEvent = {event: RuntimeEvent; sessionID: string};
type Hydration = {
  generation: number;
  sessionID: string;
  events: BufferedEvent[];
};

const emptySnapshot: RuntimeSnapshot = {
  phase: "booting",
  workspaceRoot: "",
  includeArchived: false,
  contextResources: [],
  messageFeedback: {},
  sessions: [],
  sessionSearchQuery: "",
  sessionSearchResults: [],
  selectedSessionID: "",
  hydratingSessionID: "",
  events: [],
  queuedTurns: [],
  conversation: emptyConversationSnapshot(),
  historyMoreBefore: false,
  providers: [],
  models: [],
  workspaces: [],
  selectedWorkspaceID: "",
  tools: [],
  checkpoints: [],
  agents: [],
  extensions: [],
  tracePhase: "idle",
  socketConnected: false
};

const immediateEventKinds = new Set([
  "approval.required",
  "approval.resolved",
  "input.required",
  "input.resolved",
  "operation.rejected",
  "turn.queued",
  "turn.queue.updated",
  "turn.queue.removed",
  "turn.started",
  "turn.steered",
  "turn.completed",
  "turn.failed",
  "turn.canceled",
  "turn.withdrawn",
  "turn.receipt"
]);

const progressEventKinds = new Set([
  "plan.delta",
  "agent.spawned",
  "agent.status",
  "agent.message",
  "agent.integration"
]);

const sessionActivityEventKinds = new Set([
  "session.title.updated",
  "approval.required",
  "approval.resolved",
  "input.required",
  "input.resolved",
  "turn.started",
  "turn.completed",
  "turn.failed",
  "turn.canceled",
  "turn.withdrawn"
]);

// catalogHasActivatingWorkspace：目录里存在尚未激活完成的工作区。
// ready=true 只表示 Supervisor 就绪；已注册工作区的激活可能仍在进行，
// 此时不应落入“无工作区”终态，而应继续等待。
function catalogHasActivatingWorkspace(
  catalog: WorkspaceCatalog
): boolean {
  return catalog.workspaces.length > 0 &&
    !catalog.workspaces.some((workspace) => workspace.ready);
}

export class RuntimeClient {
  private token = "";
  private cursor = 0;
  private socket?: WebSocket;
  private sessionRefreshQueued = false;
  private sessionRefreshPending = new Set<string>();
  private sessionListRequest?: {
    promise: Promise<void>;
    workspaceIDs?: ReadonlySet<string>;
  };
  private reconnectTimer?: number;
  private bootTimer?: number;
  private generation = 0;
  private sessionListGeneration = 0;
  private sessionListController?: AbortController;
  private sessionListHydrationRequested = false;
  private sessionWorkspaceIDs = new Map<string, string>();
  private selectionGeneration = 0;
  private selectionController?: AbortController;
  private progressRequest?: {
    controller: AbortController;
    promise: Promise<void>;
    dirty: boolean;
  };
  private traceTurns = new Set<string>();
  private tracePending = new Set<string>();
  private traceInFlight?: Promise<void>;
  private traceRefreshAgain = false;
  private traceWatermark = 0;
  private hydration?: Hydration;
  private historyRequest?: {
    controller: AbortController;
    promise: Promise<number>;
  };
  private state: RuntimeSnapshot = emptySnapshot;
  private listeners = new Set<Listener>();
  private conversationProjection = new ConversationProjection();
  private pendingSelectedEvents: RuntimeEvent[] = [];
  private readonly eventNotifier = new FrameNotifier(
    () => this.flushSelectedEvents()
  );
  private storageScope = "";
  private stored: BrowserProjectionState = {
    cursor: 0,
    selectedSessionID: "",
    drafts: {},
    messageFeedback: {}
  };
  private storageWrite: Promise<void> = Promise.resolve();
  private storageTimer?: number;
  private pendingStorage?: {scope: string; value: BrowserProjectionState};
  constructor(
    private readonly storage: BrowserStorage = new IndexedDBBrowserStorage()
  ) {
    if (typeof window !== "undefined") {
      window.addEventListener("pagehide", this.flushBrowserState);
      document.addEventListener("visibilitychange", this.flushWhenHidden);
    }
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): RuntimeSnapshot => this.state;

  async start(): Promise<void> {
    if (this.bootTimer !== undefined) {
      window.clearTimeout(this.bootTimer);
      this.bootTimer = undefined;
    }
    try {
      const bootstrap = await this.fetchBootstrap();
      this.token = bootstrap.token;
      if (bootstrap.draining) {
        this.update({phase: "draining", problem: bootstrap.problem});
        return;
      }
      const workspaceCatalog = workspaceCatalogFromBootstrap(bootstrap);
      const requestedWorkspaceID = typeof window === "undefined"
        ? ""
        : new URL(window.location.href).searchParams.get("workspace") ?? "";
      const candidates = requestedWorkspaceID
        ? [requestedWorkspaceID]
        : [this.state.selectedWorkspaceID];
      const selectedWorkspaceID = candidates.find((id) =>
        workspaceCatalog.workspaces.some(
        (workspace) => workspace.id === id && workspace.ready
      )) ?? "";
      const selectedWorkspace = workspaceCatalog.workspaces.find(
        (workspace) => workspace.id === selectedWorkspaceID
      );
      if (bootstrap.setup_required) {
        this.update({
          phase: "setup",
          workspaceRoot: bootstrap.workspace_root ?? "",
          workspaces: workspaceCatalog.workspaces,
          selectedWorkspaceID,
          problem: undefined
        });
        return;
      }
      if (!bootstrap.ready) {
        this.update({
          phase: bootstrap.problem ? "failed" : "booting",
          workspaceRoot: bootstrap.workspace_root ?? "",
          problem: bootstrap.problem
        });
        if (!bootstrap.problem) {
          this.bootTimer = window.setTimeout(() => void this.start(), 500);
        }
        return;
      }
      if (!selectedWorkspace && catalogHasActivatingWorkspace(workspaceCatalog)) {
        // Supervisor 已就绪但已注册工作区仍在激活：等待而不是落入
        // “无工作区”终态。
        this.update({
          phase: "booting",
          workspaceRoot: bootstrap.workspace_root ?? "",
          workspaces: workspaceCatalog.workspaces,
          problem: undefined
        });
        this.bootTimer = window.setTimeout(() => void this.start(), 500);
        return;
      }
      if (!selectedWorkspace) {
        this.socket?.close(1000, "workspace selection required");
        this.socket = undefined;
        this.update({
          phase: "ready",
          workspaceRoot: "",
          workspaces: workspaceCatalog.workspaces,
          selectedWorkspaceID: "",
          sessions: [],
          selectedSessionID: "",
          profile: undefined,
          providers: [],
          models: [],
          socketConnected: false,
          problem: undefined
        });
        return;
      }
      // 多 Workspace 激活是逐个进行的：supervisor 就绪不代表目标 Workspace
      // 已激活。选中前等待其就绪，避免启动竞态把“未就绪”变成永久错误。
      await this.awaitWorkspaceReady(selectedWorkspaceID);
      await this.restoreBrowserState(bootstrap, selectedWorkspaceID);
      this.update({
        phase: "reconnecting",
        workspaceRoot: selectedWorkspace?.root ?? bootstrap.workspace_root ?? "",
        workspaces: workspaceCatalog.workspaces,
        selectedWorkspaceID,
        problem: undefined
      });
      this.socket?.close(1000, "client reconnect");
      await this.connect();
      await this.refreshModelCatalog();
      await this.refreshSessions();
    } catch (error) {
      if (this.state.phase !== "reconnecting" &&
          this.state.phase !== "desynchronized") {
        this.fail(error);
      }
    }
  }

  stop(): void {
    this.cancelSessionRefresh();
    this.sessionListHydrationRequested = false;
    this.cancelProgressRefresh();
    this.selectionController?.abort();
    this.selectionGeneration += 1;
    this.cancelEarlierHistory();
    this.eventNotifier.flushNow();
    this.flushBrowserState();
    this.generation += 1;
    if (this.reconnectTimer !== undefined) {
      window.clearTimeout(this.reconnectTimer);
    }
    if (this.bootTimer !== undefined) {
      window.clearTimeout(this.bootTimer);
    }
    this.sessionRefreshQueued = false;
    this.socket?.close(1000, "client stopped");
    this.socket = undefined;
  }

  refreshSessions(
    query?: string,
    hydrate = query === undefined,
    includeArchived = this.state.includeArchived
  ): Promise<void> {
    return this.startSessionRefresh(query, hydrate, includeArchived);
  }

  private async startSessionRefresh(
    query: string | undefined,
    hydrate: boolean,
    includeArchived: boolean,
    workspaceIDs?: ReadonlySet<string>
  ): Promise<void> {
    // If an explicit query replaces a scoped activity read, keep its
    // invalidation so the catalog is updated after the new search finishes.
    for (const id of this.sessionListRequest?.workspaceIDs ?? []) {
      this.sessionRefreshPending.add(id);
    }
    const request = {
      promise: this.refreshSessionLists(query, hydrate, includeArchived, workspaceIDs),
      workspaceIDs
    };
    this.sessionListRequest = request;
    try {
      await request.promise;
    } finally {
      if (this.sessionListRequest === request) {
        this.sessionListRequest = undefined;
        this.queueSessionRefresh();
      }
    }
  }

  private async refreshSessionLists(
    query: string | undefined,
    hydrate: boolean,
    includeArchived: boolean,
    workspaceIDs?: ReadonlySet<string>
  ): Promise<void> {
    this.sessionListHydrationRequested ||= hydrate;
    this.sessionListController?.abort();
    const controller = new AbortController();
    this.sessionListController = controller;
    const generation = ++this.sessionListGeneration;
    const searchQuery = query ?? this.state.sessionSearchQuery;
    const searching = Boolean(searchQuery.trim());
    const loadCatalog = query === undefined || !searching ||
      this.sessionListHydrationRequested;
    if (searchQuery !== this.state.sessionSearchQuery ||
        includeArchived !== this.state.includeArchived) {
      this.update({
        sessionSearchQuery: searchQuery,
        includeArchived,
        ...(searchQuery !== this.state.sessionSearchQuery
          ? {sessionSearchResults: []} : {})
      });
    }
    const workspaces = this.state.workspaces.filter((workspace) =>
      workspace.ready && (!workspaceIDs || workspaceIDs.has(workspace.id)));
    const list = (workspace: WorkspaceDescriptor, value: string) =>
      this.call<SessionList>("session/list", {
        query: value,
        include_archived: includeArchived,
        limit: 200
      }, {workspaceID: workspace.id, signal: controller.signal});
    const requests = workspaces.map(async (workspace) => {
      const [catalog, search] = await Promise.allSettled([
        loadCatalog ? list(workspace, "") : Promise.resolve(undefined),
        searching ? list(workspace, searchQuery) : Promise.resolve(undefined)
      ]);
      return {workspace, catalog, search};
    });
    const lists = await Promise.all(requests);
    if (controller.signal.aborted || generation !== this.sessionListGeneration) return;
    // A newer background list may supersede the startup list, but it must
    // inherit the request to restore the selected session's full history.
    const hydrateSelected = this.sessionListHydrationRequested;
    this.sessionListHydrationRequested = false;
    const sessionWorkspaceIDs = new Map<string, string>();
    const sessionSearchResults: SessionSummary[] = [];
    const byWorkspace = new Map(lists.map((list) => [list.workspace.id, list]));
    const currentWorkspaces = this.state.workspaces.filter((workspace) => workspace.ready);
    const sessions = currentWorkspaces.flatMap((workspace) => {
      const {catalog, search} = byWorkspace.get(workspace.id) ?? {};
      const values = catalog?.status === "fulfilled" && catalog.value
        ? catalog.value.sessions
        : this.state.sessions.filter(
          (session) => session.workspace_root === workspace.root
        );
      const results = search?.status === "fulfilled" && search.value
        ? search.value.sessions
        : this.state.sessionSearchResults.filter(
          (session) => session.workspace_root === workspace.root
        );
      if (searching) sessionSearchResults.push(...results);
      // Searching may find an older session beyond the catalog's first page.
      // Merge its metadata without replacing the authoritative session list.
      const merged = new Map(values.map((session) => [session.session_id, session]));
      if (searching) {
        for (const session of results) merged.set(session.session_id, session);
      }
      if (searching || query !== undefined) {
        const active = this.state.sessions.find((session) =>
          session.session_id === this.state.selectedSessionID &&
          session.workspace_root === workspace.root);
        if (active && !merged.has(active.session_id)) {
          merged.set(active.session_id, active);
        }
      }
      for (const session of merged.values()) {
        sessionWorkspaceIDs.set(session.session_id, workspace.id);
      }
      return [...merged.values()];
    });
    this.sessionWorkspaceIDs = sessionWorkspaceIDs;
    const selected = this.state.selectedSessionID || this.stored.selectedSessionID;
    const preferred = sessions.filter(
      (session) => sessionWorkspaceIDs.get(session.session_id) ===
        this.state.selectedWorkspaceID
    );
    const candidates = this.state.selectedWorkspaceID ? preferred : sessions;
    const nextSelected = (query !== undefined && !hydrateSelected) ||
      (searching && Boolean(this.state.selectedSessionID))
      ? this.state.selectedSessionID
      : selected
        // 已有选中会话时保持不变：过滤（搜索/归档开关）或目录分页把它
        // 暂时挤出列表时，静默切换到其他会话会被感知为“页面自己跳走”。
        ? selected
        : (candidates[0]?.session_id ?? "");
    this.update({
      sessions,
      sessionSearchResults,
      selectedSessionID: nextSelected,
      includeArchived
    });
    if (hydrateSelected && nextSelected && (!this.state.profile || nextSelected !== selected)) {
      await this.selectSession(nextSelected);
    }
  }

  async setArchivedVisible(includeArchived: boolean): Promise<void> {
    await this.refreshSessions(undefined, true, includeArchived);
  }

  private cancelSessionRefresh(): void {
    this.sessionListController?.abort();
    this.sessionListGeneration += 1;
    this.sessionListRequest = undefined;
    this.sessionRefreshPending.clear();
  }

  async refreshWorkspaces(): Promise<WorkspaceCatalog> {
    const catalog = await this.call<WorkspaceCatalog>("workspace/list", {});
    this.update({workspaces: catalog.workspaces ?? []});
    return catalog;
  }

  async addWorkspace(path: string): Promise<void> {
    const result = await this.call<WorkspaceAddResult>(
      "workspace/add",
      {path: path.trim()},
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.refreshWorkspaces();
    if (!result.workspace.ready) {
      if (this.state.phase === "setup") {
        // Workspace 已登记；模型配置完成前 Runtime 无法激活，
        // 配置成功后由服务端自动激活，这里不视为错误。
        return;
      }
      throw new Error(
        result.workspace.problem || "Workspace was registered but is not ready"
      );
    }
    await this.selectWorkspace(result.workspace.id);
  }

  async pickWorkspaceDirectory(): Promise<WorkspaceDirectoryResult> {
    return this.call<WorkspaceDirectoryResult>(
      "workspace/select-directory",
      {}
    );
  }

  async removeWorkspace(workspaceID: string): Promise<void> {
    const removingSelected = workspaceID === this.state.selectedWorkspaceID;
    const catalog = await this.call<WorkspaceCatalog>(
      "workspace/remove", {workspace_id: workspaceID},
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    this.update({workspaces: catalog.workspaces});
    if (!removingSelected) return;

    const fallback = catalog.workspaces.find((workspace) => workspace.ready);
    if (fallback) {
      await this.switchWorkspace(fallback.id, true);
      return;
    }
    this.cancelSessionRefresh();
    this.cancelEarlierHistory();
    this.eventNotifier.cancel();
    this.pendingSelectedEvents = [];
    this.hydration = undefined;
    this.cancelProgressRefresh();
    this.selectionGeneration += 1;
    this.generation += 1;
    this.socket?.close(1000, "last workspace removed");
    this.socket = undefined;
    this.update({
      phase: "ready",
      workspaceRoot: "",
      selectedWorkspaceID: "",
      sessions: [],
      sessionSearchResults: [],
      selectedSessionID: "",
      hydratingSessionID: "",
      events: [],
      conversation: this.replaceConversation([]),
      historyMoreBefore: false,
      queuedTurns: [],
      profile: undefined,
      providers: [],
      models: [],
      tools: [],
      checkpoints: [],
      plan: undefined,
      agents: [],
      trace: undefined,
      tracePhase: "idle",
      traceProblem: undefined,
      extensions: [],
      mergePlan: undefined,
      contextResources: [],
      messageFeedback: {},
      socketConnected: false,
      problem: undefined
    });
  }

  async switchWorkspaceBranch(
    workspaceID: string,
    branch: string
  ): Promise<void> {
    const git = await this.call<WorkspaceGitState>(
      "workspace/git-switch",
      {branch},
      {
        workspaceID,
        idempotencyKey: crypto.randomUUID(),
        retryNetwork: true
      }
    );
    this.update({
      workspaces: this.state.workspaces.map((workspace) =>
        workspace.id === workspaceID ? {...workspace, git} : workspace
      )
    });
  }

  async gitOverview(workspaceID: string, signal?: AbortSignal): Promise<GitOverview> {
    return this.call("workspace/git-status", {}, {workspaceID, signal});
  }

  async gitPatch(workspaceID: string, path: string, staged: boolean, signal?: AbortSignal): Promise<GitPatch> {
    return this.call("workspace/git-diff", {path, staged}, {workspaceID, signal});
  }

  async executeGitAction(workspaceID: string, sessionID: string | undefined, action: GitActionRequest): Promise<GitActionResult> {
    const session = this.state.sessions.find((item) => item.session_id === sessionID);
    if (workspaceID !== this.state.selectedWorkspaceID ||
        sessionID && (sessionID !== this.state.selectedSessionID ||
        !session || this.workspaceIDForSession(sessionID) !== workspaceID ||
        session.isolation !== "shared" || session.archived)) {
      throw new Error("The selected Workspace or Session changed.");
    }
    if (session && !["idle", "completed", "failed"].includes(session.status) ||
        this.state.sessions.some((item) =>
          this.workspaceIDForSession(item.session_id) === workspaceID &&
          ["running", "awaiting_approval", "awaiting_input"].includes(item.status)) ||
        this.state.profile?.profile.mode === "plan" || this.state.profile?.profile.approval_posture === "never") {
      throw new Error("Finish or resume current work before changing Git state.");
    }
    if (!action.branch || !action.revision || (action.action === "commit" || action.action === "commit_push") && (!action.paths?.length || !action.message?.trim()) ||
        (action.action === "push" || action.action === "commit_push") && !action.remote ||
        action.action === "create_branch" && !action.new_branch?.trim()) {
      throw new Error("Git request is incomplete.");
    }
    return this.call<GitActionResult>("workspace/git-action", {
      ...action, session_id: sessionID
    }, {workspaceID, idempotencyKey: crypto.randomUUID()});
  }

  async selectWorkspace(workspaceID: string): Promise<void> {
    await this.switchWorkspace(workspaceID, true);
  }

  private async switchWorkspace(
    workspaceID: string,
    hydrate: boolean
  ): Promise<void> {
    const workspace = this.state.workspaces.find(
      (entry) => entry.id === workspaceID
    );
    if (!workspace) throw new Error("Workspace is not registered");
    if (!workspace.ready) {
      throw new Error(workspace.problem || "Workspace Runtime is not ready");
    }
    if (workspaceID === this.state.selectedWorkspaceID) {
      if (hydrate) await this.refreshSessions();
      return;
    }
    this.cancelSessionRefresh();
    this.cancelEarlierHistory();
    this.eventNotifier.cancel();
    this.pendingSelectedEvents = [];
    this.hydration = undefined;
    this.cancelProgressRefresh();
    this.selectionGeneration += 1;
    this.generation += 1;
    this.socket?.close(1000, "workspace changed");
    this.socket = undefined;
    this.flushBrowserState();
    await this.storageWrite;
    await this.restoreBrowserState({
      protocol_version: this.protocolVersion,
      server_build: this.serverBuild,
      token: this.token,
      ready: true,
      draining: false
    }, workspaceID);
    this.update({
      phase: "reconnecting",
      selectedWorkspaceID: workspaceID,
      workspaceRoot: workspace.root,
      hydratingSessionID: "",
      events: [],
      conversation: this.replaceConversation([]),
      queuedTurns: [],
      profile: undefined,
      tools: [],
      checkpoints: [],
      plan: undefined,
      agents: [],
      usage: undefined,
      trace: undefined,
      tracePhase: "idle",
      extensions: [],
      contextResources: []
    });
    if (typeof window !== "undefined") {
      const target = new URL(window.location.href);
      target.searchParams.set("workspace", workspaceID);
      window.history.replaceState(null, "", target);
    }
    await this.connect();
    await this.refreshModelCatalog();
    await this.refreshSessions(undefined, hydrate);
  }

  async createSession(
    isolation: "shared" | "worktree" = "shared",
    profilePatch?: Record<string, unknown>
  ): Promise<void> {
    const workspace = this.state.workspaces.find(
      (value) => value.id === this.state.selectedWorkspaceID && value.ready
    );
    if (!workspace) {
      throw new Error("Select a ready workspace before creating a session");
    }
    const approval = this.state.profile?.profile.approval_posture;
    profilePatch = {
      ...(approval ? {approval_posture: approval} : {}),
      ...profilePatch
    };
    const idempotencyKey = crypto.randomUUID();
    const sessionID = `session_web_${idempotencyKey}`;
    const binding = await this.call<SessionBinding>(
      "session/create",
      {session_id: sessionID, isolation},
      {idempotencyKey, retryNetwork: true}
    );
    await this.refreshSessions(undefined, false);
    await this.selectSession(binding.session_id);
    if (profilePatch && Object.keys(profilePatch).length > 0) {
      await this.updateProfile(profilePatch);
    }
  }

  async completeSetup(request: SetupRequest): Promise<void> {
    const result = await this.call<SetupResult>(
      "setup/apply",
      request,
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    if (!result.ready) throw new Error("Runtime setup did not become ready");
    await this.start();
  }

  async probeSetup(request: SetupProbeRequest): Promise<SetupProbeResult> {
    return this.call<SetupProbeResult>("setup/probe", request);
  }

  async updateSession(
    sessionID: string,
    expectedRevision: number,
    patch: {title?: string; pinned?: boolean; archived?: boolean}
  ): Promise<void> {
    await this.call<SessionLifecycleUpdate>("session/update", {
      session_id: sessionID,
      expected_revision: expectedRevision,
      patch
    }, {workspaceID: this.workspaceIDForSession(sessionID)});
    await this.refreshSessions();
  }

  async deleteSession(
    sessionID: string,
    expectedRevision: number,
    discard = false
  ): Promise<void> {
    await this.call<SessionDeleteResult>("session/delete", {
      session_id: sessionID,
      expected_revision: expectedRevision,
      discard
    }, {workspaceID: this.workspaceIDForSession(sessionID)});
    if (this.state.selectedSessionID === sessionID) {
      this.cancelProgressRefresh();
      this.selectionGeneration += 1;
      this.hydration = undefined;
      this.update({
        selectedSessionID: "",
        hydratingSessionID: "",
        events: [],
        conversation: this.replaceConversation([]),
        queuedTurns: [],
        historyMoreBefore: false,
        profile: undefined,
        tools: [],
        checkpoints: [],
        plan: undefined,
        agents: [],
        usage: undefined,
        trace: undefined,
        tracePhase: "idle",
        traceProblem: undefined,
        mergePlan: undefined,
        contextResources: []
      });
    }
    await this.refreshSessions();
  }

  async selectSession(sessionID: string): Promise<void> {
    const ownerWorkspaceID = this.workspaceIDForSession(sessionID);
    const ownerWorkspace = this.state.workspaces.find(
      (workspace) => workspace.id === ownerWorkspaceID
    );
    if (ownerWorkspace && ownerWorkspace.id !== this.state.selectedWorkspaceID) {
      await this.switchWorkspace(ownerWorkspace.id, false);
    }
    const workspaceID = ownerWorkspace?.id ?? this.state.selectedWorkspaceID;
    this.cancelEarlierHistory();
    this.eventNotifier.cancel();
    this.pendingSelectedEvents = [];
    const previousSessionID = this.state.selectedSessionID;
    this.cancelProgressRefresh();
    const generation = ++this.selectionGeneration;
    this.selectionController?.abort();
    const controller = new AbortController();
    this.selectionController = controller;
    const options = {workspaceID, signal: controller.signal};
    this.traceTurns.clear();
    this.tracePending.clear();
    this.traceInFlight = undefined;
    this.traceWatermark = 0;
    const hydration: Hydration = {generation, sessionID, events: []};
    this.hydration = hydration;
    this.update({
      selectedSessionID: sessionID,
      hydratingSessionID: sessionID,
      events: [],
      conversation: this.replaceConversation([]),
      queuedTurns: [],
      historyMoreBefore: false,
      profile: undefined,
      tools: [],
      checkpoints: [],
      plan: undefined,
      agents: [],
      usage: undefined,
      trace: undefined,
      tracePhase: "loading",
      traceProblem: undefined,
      extensions: [],
      mergePlan: undefined,
      contextResources: [],
      problem: undefined
    });
    try {
    const summary = this.state.sessions.find((item) => item.session_id === sessionID);
    await this.call<SessionBinding>("session/activate", {
      session_id: sessionID,
      thread_id: summary?.thread_id
    }, options);
    if (generation !== this.selectionGeneration) return;
    const snapshot = await this.call<PresentationSnapshot>("session/snapshot", {
      session_id: sessionID
    }, options);
    if (generation !== this.selectionGeneration) return;
    const snapshotEvents = snapshot.events ?? [];
    const liveEvents = hydration.events
      .filter(({event, sessionID: owner}) =>
        owner === sessionID && event.sequence > snapshot.through_sequence
      )
      .map(({event}) => event)
      .sort((left, right) => left.sequence - right.sequence);
    const latestBufferedSequence = hydration.events.reduce(
      (latest, {event}) => Math.max(latest, event.sequence),
      0
    );
    const refreshForForeignEvent = hydration.events.some(
      ({sessionID: owner}) => owner !== sessionID
    );
    const refreshForDefaultTitle = summary?.title_source === "default" &&
      snapshotEvents.some((event) => event.kind === "turn.started");
    const refreshForTitle = hydration.events.some(
      ({event}) => event.kind === "session.title.updated"
    );
    this.commitCursor(Math.max(
      snapshot.through_sequence,
      latestBufferedSequence
    ));
    this.hydration = undefined;
    const events = [...(snapshot.events ?? []), ...liveEvents];
    this.update({
      selectedSessionID: sessionID,
      events,
      conversation: this.replaceConversation(events),
      historyMoreBefore: Boolean(snapshot.history_truncated_before),
      mergePlan: undefined,
      queuedTurns: projectTurnQueue([], liveEvents),
      contextResources: [],
      problem: undefined
    });
    this.persistSelectedSession(sessionID);
    if (refreshForForeignEvent || refreshForDefaultTitle || refreshForTitle) {
      void this.refreshSessions(undefined, false);
    }
    this.traceWatermark = snapshot.through_sequence;
    const current = () => generation === this.selectionGeneration &&
      !controller.signal.aborted && sessionID === this.state.selectedSessionID;
    const detail = async <T>(
      route: WebRPCRoute, body: unknown, project: (value: T) => Partial<RuntimeSnapshot>
    ): Promise<void> => {
      const requestedAt = this.cursor;
      const value = await this.call<T>(route, body, options);
      if (!current()) return;
      this.eventNotifier.flushNow();
      // Live events already trigger authoritative refreshes for these panels.
      // A slower initial response must not overwrite their newer projection.
      const advanced = this.state.events.some((event) =>
        event.sequence > requestedAt &&
        route === "usage/query" && isTerminal(event.kind)
      );
      if (!advanced) this.update(project(value));
    };
    const controls = Promise.all([
      detail<SessionProfileSnapshot>("profile/get", {session_id: sessionID},
        (profile) => ({profile})),
      detail<TurnQueue>("turn/queue", {session_id: sessionID}, (queue) => ({
        queuedTurns: projectTurnQueue(queue.items ?? [],
          this.state.events.filter((event) => event.sequence > snapshot.through_sequence))
      }))
    ]).then(() => {
      if (current()) {
        this.eventNotifier.flushNow();
        this.update({hydratingSessionID: ""});
      }
    }).catch((error: unknown) => {
      if (current()) this.update({problem: {
        version: 1, code: "unavailable", message: errorMessage(error), retryable: true
      }});
    });
    await Promise.allSettled([
      controls,
      detail<ToolCatalog>("tool/catalog", {session_id: sessionID},
        (catalog) => ({tools: catalog.tools ?? []})),
      detail<CheckpointList>("checkpoint/list", {session_id: sessionID, limit: 20},
        (value) => ({checkpoints: value.checkpoints ?? []})),
      this.refreshProgress(sessionID),
      detail<UsageQueryResult>("usage/query",
        {session_id: sessionID, include_children: true, limit: 100},
        (value) => ({usage: value.rollup})),
      detail<ExtensionControlResult>("extension/list", {kind: "all"},
        (value) => ({extensions: value.extensions ?? []})),
      this.refreshTrace(sessionID)
    ]);
    } catch (error) {
      if (generation !== this.selectionGeneration || this.hydration !== hydration) {
        return;
      }
      if (this.hydration === hydration) {
        this.hydration = undefined;
        this.update({
          selectedSessionID: previousSessionID,
          hydratingSessionID: ""
        });
      }
      throw error;
    }
  }

  async submitPrompt(prompt: string): Promise<OperationReceipt> {
    const sessionID = this.requireSession();
    const key = crypto.randomUUID();
    const receipt = await this.call<OperationReceipt>("operation/submit", {
      session_id: sessionID,
      kind: "turn.start",
      idempotency_key: key,
      payload: {
        prompt,
        display_prompt: prompt,
        intent: this.state.profile?.profile.mode === "plan" ? "plan" : "answer",
        context: this.state.contextResources
      }
    });
    this.update({contextResources: []});
    await this.refreshSessions(undefined, false);
    return receipt;
  }

  loadDraft(sessionID = this.state.selectedSessionID): string {
    if (!sessionID) return "";
    return this.stored.drafts[sessionID] ?? "";
  }

  saveDraft(
    value: string,
    sessionID = this.state.selectedSessionID
  ): void {
    if (!sessionID) return;
    const drafts = {...this.stored.drafts};
    if (value) {
      drafts[sessionID] = value;
    } else {
      delete drafts[sessionID];
    }
    this.stored = {...this.stored, drafts};
    this.persistBrowserState();
  }

  toggleMessageFeedback(
    messageID: string,
    rating: "positive" | "negative",
    sessionID = this.state.selectedSessionID
  ): void {
    if (!sessionID || !messageID) return;
    const key = `${sessionID}:${messageID}`;
    const messageFeedback = {...(this.stored.messageFeedback ?? {})};
    if (messageFeedback[key] === rating) {
      delete messageFeedback[key];
    } else {
      messageFeedback[key] = rating;
    }
    this.stored = {...this.stored, messageFeedback};
    this.update({messageFeedback});
    this.persistBrowserState();
  }

  async compactThread(): Promise<OperationReceipt> {
    const session = this.state.sessions.find(
      (item) => item.session_id === this.state.selectedSessionID
    );
    const turnID = session?.latest_turn_id;
    if (!session || !turnID || this.state.conversation.activeTurnID) {
      throw new Error("No completed turn is available to compact");
    }
    return this.call<OperationReceipt>("operation/submit", {
      session_id: session.session_id,
      kind: "thread.compact",
      idempotency_key: crypto.randomUUID(),
      payload: {
        thread_id: session.thread_id,
        turn_id: turnID,
        item_id: `compact-${crypto.randomUUID()}`
      }
    });
  }

  async cancel(turnID: string): Promise<OperationReceipt> {
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.cancel",
      idempotency_key: crypto.randomUUID(),
      payload: {turn_id: turnID, reason: "user_interrupted"}
    });
  }

  async withdrawTurn(turnID: string): Promise<void> {
    const sessionID = this.requireSession();
    const workspaceID = this.workspaceIDForSession(sessionID);
    await this.call("turn/withdraw", {
      session_id: sessionID,
      turn_id: turnID
    }, {workspaceID});
    await this.refreshSessions();
    if (this.state.selectedSessionID === sessionID) {
      await this.selectSession(sessionID);
    }
  }

  async steer(turnID: string, prompt: string): Promise<OperationReceipt> {
    const normalized = prompt.trim();
    if (!normalized) throw new Error("Steering prompt is required");
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.steer",
      idempotency_key: crypto.randomUUID(),
      payload: {turn_id: turnID, prompt: normalized}
    });
  }

  async enqueue(turnID: string, prompt: string): Promise<OperationReceipt> {
    const normalized = prompt.trim();
    if (!normalized) throw new Error("Queued prompt is required");
    const receipt = await this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.enqueue",
      idempotency_key: crypto.randomUUID(),
      payload: {
        turn_id: turnID,
        prompt: normalized,
        display_prompt: normalized,
        intent: this.state.profile?.profile.mode === "plan" ? "plan" : "answer",
        context: this.state.contextResources
      }
    });
    this.update({contextResources: []});
    return receipt;
  }

  async updateQueuedTurn(queueID: string, prompt: string): Promise<OperationReceipt> {
    const item = this.requireQueuedTurn(queueID);
    const normalized = prompt.trim();
    if (!normalized) throw new Error("Queued prompt is required");
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.queue.update",
      idempotency_key: crypto.randomUUID(),
      payload: {
        thread_id: item.thread_id,
        turn_id: item.source_turn_id,
        queue_id: queueID,
        prompt: normalized,
        display_prompt: normalized
      }
    });
  }

  async removeQueuedTurn(queueID: string): Promise<OperationReceipt> {
    const item = this.requireQueuedTurn(queueID);
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.queue.remove",
      idempotency_key: crypto.randomUUID(),
      payload: {
        thread_id: item.thread_id,
        turn_id: item.source_turn_id,
        queue_id: queueID
      }
    });
  }

  async promoteQueuedTurn(
    queueID: string,
    turnID: string
  ): Promise<OperationReceipt> {
    const item = this.requireQueuedTurn(queueID);
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "turn.queue.promote",
      idempotency_key: crypto.randomUUID(),
      payload: {
        thread_id: item.thread_id,
        turn_id: turnID,
        queue_id: queueID
      }
    });
  }

  async decideApproval(
    requestID: string,
    decision: "approve" | "deny" | "cancel",
    planID = "",
    scope = "",
    replacementArguments?: Record<string, unknown>
  ): Promise<OperationReceipt> {
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "approval.decision",
      idempotency_key: crypto.randomUUID(),
      payload: {
        request_id: requestID,
        decision,
        plan_id: planID,
        scope,
        replacement_arguments: replacementArguments
      }
    });
  }

  async replyInput(
    requestID: string,
    answer: string,
    values?: Record<string, string>
  ): Promise<OperationReceipt> {
    return this.call<OperationReceipt>("operation/submit", {
      session_id: this.requireSession(),
      kind: "input.reply",
      idempotency_key: crypto.randomUUID(),
      payload: {request_id: requestID, answer, values}
    });
  }

  async recoverTurn(
    sourceTurnID: string,
    action: "retry" | "continue",
    prompt = ""
  ): Promise<OperationReceipt> {
    return this.call<OperationReceipt>("turn/recover", {
      version: 1,
      action,
      session_id: this.requireSession(),
      source_turn_id: sourceTurnID,
      prompt,
      idempotency_key: crypto.randomUUID()
    });
  }

  async updateProfile(
    patch: Record<string, unknown>
  ): Promise<SessionProfileUpdateResult> {
    const generation = this.selectionGeneration;
    const snapshot = this.state.profile;
    const profile = snapshot?.profile;
    const session = this.state.sessions.find(
      (item) => item.session_id === this.state.selectedSessionID
    );
    if (!profile || !session) {
      throw new Error("No active session");
    }
    const result = await this.call<SessionProfileUpdateResult>("profile/update", {
      session_id: session.session_id,
      thread_id: session.thread_id,
      expected_revision: profile.revision,
      patch
    });
    const [authoritative, catalog] = await Promise.all([
      this.call<SessionProfileSnapshot>("profile/get", {
        session_id: session.session_id
      }),
      this.call<ToolCatalog>("tool/catalog", {
        session_id: session.session_id
      })
    ]);
    if (generation !== this.selectionGeneration ||
        session.session_id !== this.state.selectedSessionID) {
      return result;
    }
    this.update({profile: authoritative, tools: catalog.tools ?? []});
    await this.refreshModelCatalog();
    return {...result, profile: authoritative.profile};
  }

  async listAgentPresets(): Promise<AgentPresetList> {
    return this.call<AgentPresetList>("agent-preset/list", {
      session_id: this.requireSession()
    });
  }

  async saveAgentPreset(input: {
    id?: string;
    expectedRevision?: number;
    name: string;
    description?: string;
    profile: AgentPresetProfile;
  }): Promise<AgentPresetMutationResult> {
    const id = input.id || `preset-${crypto.randomUUID()}`;
    return this.call<AgentPresetMutationResult>("agent-preset/save", {
      session_id: this.requireSession(),
      id,
      expected_revision: input.expectedRevision ?? 0,
      name: input.name,
      description: input.description ?? "",
      profile: input.profile
    }, {
      idempotencyKey: id,
      retryNetwork: true
    });
  }

  async deleteAgentPreset(
    preset: Pick<AgentPreset, "id" | "revision">
  ): Promise<AgentPresetMutationResult> {
    return this.call<AgentPresetMutationResult>("agent-preset/delete", {
      session_id: this.requireSession(),
      id: preset.id,
      expected_revision: preset.revision
    });
  }

  async applyAgentPreset(presetID: string): Promise<AgentPresetApplyResult> {
    const session = this.state.sessions.find(
      (item) => item.session_id === this.state.selectedSessionID
    );
    const profile = this.state.profile?.profile;
    if (!session || !profile) throw new Error("No active session");
    const result = await this.call<AgentPresetApplyResult>("agent-preset/apply", {
      session_id: session.session_id,
      thread_id: session.thread_id,
      preset_id: presetID,
      expected_profile_revision: profile.revision
    });
    const [authoritative, catalog] = await Promise.all([
      this.call<SessionProfileSnapshot>("profile/get", {
        session_id: session.session_id
      }),
      this.call<ToolCatalog>("tool/catalog", {
        session_id: session.session_id
      })
    ]);
    this.update({profile: authoritative, tools: catalog.tools ?? []});
    return result;
  }

  loadEarlierHistory(limit = 200): Promise<number> {
    if (this.historyRequest) return this.historyRequest.promise;
    this.eventNotifier.flushNow();
    const sessionID = this.requireSession();
    const workspaceID = this.state.selectedWorkspaceID;
    const generation = this.selectionGeneration;
    const before = this.state.events[0]?.sequence;
    if (!before || !this.state.historyMoreBefore || this.hydration) return Promise.resolve(0);
    const request = {controller: new AbortController(), promise: Promise.resolve(0)};
    this.historyRequest = request;
    const current = () => !request.controller.signal.aborted &&
      generation === this.selectionGeneration &&
      workspaceID === this.state.selectedWorkspaceID &&
      sessionID === this.state.selectedSessionID;
    request.promise = (async () => {
      try {
        const page = await this.call<SessionHistoryPage>("session/history", {
          session_id: sessionID, before_sequence: before, limit
        }, {workspaceID, signal: request.controller.signal});
        if (!current()) return 0;
        this.eventNotifier.flushNow();
        const known = new Set(this.state.events.map((event) => event.sequence));
        const earlier = page.events.filter((event) => {
          if (event.sequence >= before || known.has(event.sequence)) return false;
          known.add(event.sequence);
          return true;
        }).sort((left, right) => left.sequence - right.sequence);
        if (earlier.length === 0 && page.more_before) {
          throw new Error("History did not advance. Retry loading earlier messages.");
        }
        const events = [...earlier, ...this.state.events];
        this.update({
          events,
          conversation: earlier.length > 0 ? this.replaceConversation(events) : this.state.conversation,
          historyMoreBefore: Boolean(page.more_before)
        });
        if (earlier.length > 0) void this.refreshTrace(sessionID);
        return earlier.length;
      } catch (error) {
        if (!current()) return 0;
        throw error;
      } finally {
        if (this.historyRequest === request) this.historyRequest = undefined;
      }
    })();
    return request.promise;
  }

  private cancelEarlierHistory(): void {
    this.selectionController?.abort();
    this.historyRequest?.controller.abort();
    this.historyRequest = undefined;
  }

  async setToolEnabled(toolID: string, enabled: boolean): Promise<void> {
    const current = this.state.profile?.profile.enabled_tool_ids ?? [];
    const enabledToolIDs = enabled
      ? [...new Set([...current, toolID])]
      : current.filter((id) => id !== toolID);
    await this.updateProfile({enabled_tool_ids: enabledToolIDs});
  }

  async restoreCheckpoint(checkpointID: string): Promise<void> {
    await this.call("checkpoint/restore", {
      session_id: this.requireSession(),
      checkpoint_id: checkpointID
    });
    await this.selectSession(this.requireSession());
  }

  async forkCheckpoint(checkpointID: string): Promise<void> {
    const result = await this.call<CheckpointForkResult>("checkpoint/fork", {
      session_id: this.requireSession(),
      checkpoint_id: checkpointID,
      title: "Checkpoint Fork"
    });
    await this.refreshSessions();
    await this.selectSession(result.session_id);
  }

  async setExtensionEnabled(
    kind: "skill",
    name: string,
    enabled: boolean
  ): Promise<ExtensionControlResult> {
    const mutation = await this.call<ExtensionControlResult>("extension/control", {
      version: 1,
      id: `extop-${crypto.randomUUID()}`,
      kind,
      action: enabled ? "enable" : "disable",
      name,
      created_at: new Date().toISOString()
    });
    const result = await this.call<ExtensionControlResult>("extension/list", {kind: "all"});
    this.update({extensions: result.extensions ?? []});
    return {...mutation, extensions: result.extensions ?? mutation.extensions};
  }

  async controlExtension(
    kind: "skill",
    name: string,
    action: ExtensionControlAction
  ): Promise<ExtensionControlResult> {
    const result = await this.call<ExtensionControlResult>("extension/control", {
      version: 1,
      id: `extop-${crypto.randomUUID()}`,
      kind,
      action,
      name,
      created_at: new Date().toISOString()
    });
    if (!["detail", "health", "permissions", "receipts"].includes(action)) {
      const refreshed = await this.call<ExtensionControlResult>(
        "extension/list",
        {kind: "all"}
      );
      this.update({extensions: refreshed.extensions ?? []});
    }
    return result;
  }

  async previewMerge(): Promise<void> {
    const result = await this.call<SessionMergeResult>("session/merge", {
      session_id: this.requireSession(),
      action: "preview"
    });
    this.update({mergePlan: result.plan});
  }

  async applyMerge(): Promise<void> {
    if (!this.state.mergePlan) {
      throw new Error("No merge preview");
    }
    await this.call<SessionMergeResult>("session/merge", {
      session_id: this.requireSession(),
      action: "apply",
      plan_id: this.state.mergePlan.id
    });
    this.update({mergePlan: undefined});
    await this.refreshSessions();
  }

  async browseWorkspace(path = "."): Promise<WorkspaceBrowseResult> {
    return this.call<WorkspaceBrowseResult>("workspace/browse", {path, limit: 200});
  }

  async searchWorkspace(query: string): Promise<WorkspaceSearchResult> {
    return this.call<WorkspaceSearchResult>("workspace/search", {query, limit: 100});
  }

  async readWorkspaceResource(path: string): Promise<WorkspaceResource> {
    return this.call<WorkspaceResource>("workspace/resource", {path});
  }

  async readWorkspaceImage(path: string): Promise<WorkspaceImage> {
    return this.call<WorkspaceImage>("workspace/image", {path});
  }

  async searchWorkspaceSymbols(
    query: string,
    path = ""
  ): Promise<WorkspaceSymbolList> {
    return this.call<WorkspaceSymbolList>("workspace/symbols", {
      query,
      path,
      limit: 100
    });
  }

  async workspaceDiagnostics(): Promise<WorkspaceDiagnostics> {
    return this.call<WorkspaceDiagnostics>("workspace/diagnostics", {
      session_id: this.requireSession()
    });
  }

  async downloadWorkspaceContent(handle: string): Promise<Blob> {
    if (!handle) throw new Error("Content handle is required");
    const response = await fetch(`/api/v1/content/${encodeURIComponent(handle)}`, {
      method: "GET",
      headers: {
        "Authorization": `Bearer ${this.token}`,
        "X-QCode-Workspace-ID": this.state.selectedWorkspaceID
      },
      credentials: "same-origin"
    });
    if (!response.ok) {
      throw new Error(`Content download failed (${response.status})`);
    }
    return response.blob();
  }

  async workspaceDiff(): Promise<WorkspaceDiff> {
    return this.call<WorkspaceDiff>("workspace/diff", {
      session_id: this.requireSession()
    });
  }

  addWorkspaceContext(resource: WorkspaceResource, range?: EditorRange): void {
    const resources = this.state.contextResources.filter(
      (value) => value.path !== resource.path
    );
    this.update({
      contextResources: [
        ...resources,
        {
          kind: range ? "selection" : "file",
          source: "composer",
          uri: resource.uri,
          path: resource.path,
          document_version: resource.document_version,
          digest: resource.digest,
          range,
          explicit: true
        }
      ]
    });
  }

  addImageContext(image: WorkspaceImage): void {
    const resources = this.state.contextResources.filter(
      (value) => value.kind !== "image" || value.path !== image.path
    );
    this.update({
      contextResources: [
        ...resources,
        {
          kind: "image",
          source: "native_picker",
          uri: image.uri,
          path: image.path,
          document_version: image.document_version,
          digest: image.digest,
          label: image.label,
          media_type: image.media_type,
          explicit: true
        }
      ]
    });
  }

  addAttachmentContext(reference: EditorContextReference): void {
    if (reference.kind !== "attachment" &&
        !(reference.kind === "image" && !reference.path)) {
      throw new Error("Composer attachment context is invalid");
    }
    const resources = this.state.contextResources.filter(
      (value) => value.digest !== reference.digest
    );
    this.update({contextResources: [...resources, reference]});
  }

  removeAttachmentContext(digest: string): void {
    this.update({
      contextResources: this.state.contextResources.filter(
        (value) => value.digest !== digest
      )
    });
  }

  addSymbolContext(symbol: WorkspaceSymbol): void {
    const resources = this.state.contextResources.filter(
      (value) =>
        value.kind !== "symbol" ||
        value.path !== symbol.path ||
        value.symbol?.name !== symbol.name
    );
    this.update({
      contextResources: [
        ...resources,
        {
          kind: "symbol",
          source: "native_picker",
          uri: symbol.uri,
          path: symbol.path,
          document_version: symbol.document_version,
          digest: symbol.digest,
          range: symbol.range,
          symbol: {
            name: symbol.name,
            kind: symbol.kind,
            selection_range: symbol.selection_range
          },
          explicit: true
        }
      ]
    });
  }

  addDiagnosticsContext(value: WorkspaceDiagnosticContext): void {
    const context = value.context;
    const resources = this.state.contextResources.filter(
      (resource) =>
        resource.kind !== "diagnostics" ||
        resource.path !== context.path
    );
    this.update({contextResources: [...resources, context]});
  }

  addGitDiffContext(diff: WorkspaceDiff): void {
    const resources = this.state.contextResources.filter(
      (value) => value.kind !== "git_diff"
    );
    this.update({
      contextResources: [
        ...resources,
        {
          kind: "git_diff",
          source: "composer",
          digest: diff.digest,
          label: "Current workspace diff",
          media_type: "text/plain",
          content: diff.diff,
          explicit: true
        }
      ]
    });
  }

  async addTerminalContext(callID: string, content: string): Promise<void> {
    if (!callID || !content) throw new Error("Tool output context is incomplete");
    const digest = await sha256Hex(content);
    const resources = this.state.contextResources.filter(
      (value) => value.kind !== "terminal" || value.label !== callID
    );
    this.update({
      contextResources: [
        ...resources,
        {
          kind: "terminal",
          source: "composer",
          digest,
          label: callID,
          media_type: "text/plain",
          content,
          explicit: true
        }
      ]
    });
  }

  removeContext(
    kind: EditorContextReference["kind"],
    path = "",
    label = "",
    symbolName = ""
  ): void {
    this.update({
      contextResources: this.state.contextResources.filter(
        (resource) =>
          resource.kind !== kind ||
          (resource.path ?? "") !== path ||
          (resource.label ?? "") !== label ||
          (symbolName !== "" && resource.symbol?.name !== symbolName)
      )
    });
  }

  async diagnostics(): Promise<Record<string, unknown>> {
    return this.call<Record<string, unknown>>("system/diagnostics", {});
  }

  async credentialStatus(): Promise<CredentialStatus> {
    return this.call<CredentialStatus>("credential/status", {});
  }

  async connectionStatus(): Promise<WorkspaceConnection> {
    return this.call<WorkspaceConnection>("connection/status", {});
  }

  async listConnections(): Promise<ConnectionListResult> {
    return this.call<ConnectionListResult>("connection/list", {});
  }

  async addConnection(request: SetupRequest): Promise<ConnectionListResult> {
    const result = await this.call<ConnectionListResult>(
      "connection/add",
      request,
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.start();
    return result;
  }

  async removeConnection(connectionID: string): Promise<ConnectionListResult> {
    const result = await this.call<ConnectionListResult>(
      "connection/remove",
      {connection_id: connectionID},
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.start();
    return result;
  }

  async setDefaultConnection(connectionID: string): Promise<ConnectionListResult> {
    const result = await this.call<ConnectionListResult>(
      "connection/default",
      {connection_id: connectionID},
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.start();
    return result;
  }

  async setKeyringCredential(secret: string): Promise<CredentialStatus> {
    return this.call<CredentialStatus>("credential/set-keyring", {secret});
  }

  async clearKeyringCredential(): Promise<CredentialStatus> {
    return this.call<CredentialStatus>("credential/clear-keyring", {});
  }

  async validateCredential(): Promise<CredentialStatus> {
    return this.call<CredentialStatus>("credential/validate", {});
  }

  async testModel(model: string): Promise<ModelTestResult> {
    return this.call<ModelTestResult>("model/test", {model});
  }

  async probeModel(model: string): Promise<SetupProbeResult> {
    return this.call<SetupProbeResult>("model/probe", {model});
  }

  async addModel(request: ModelMutationRequest): Promise<ModelCatalog> {
    const result = await this.call<ModelCatalog>(
      "model/add",
      request,
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.acceptModelCatalog(result);
    return result;
  }

  async updateModel(request: ModelMutationRequest): Promise<ModelCatalog> {
    const result = await this.call<ModelCatalog>(
      "model/update",
      request,
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.acceptModelCatalog(result);
    return result;
  }

  async removeModel(model: string): Promise<ModelCatalog> {
    const result = await this.call<ModelCatalog>(
      "model/remove",
      {model},
      {idempotencyKey: crypto.randomUUID(), retryNetwork: true}
    );
    await this.acceptModelCatalog(result);
    return result;
  }

  private async acceptModelCatalog(result: ModelCatalog): Promise<void> {
    const sessionID = this.state.selectedSessionID;
    const profile = sessionID
      ? await this.call<SessionProfileSnapshot>("profile/get", {
          session_id: sessionID
        })
      : undefined;
    this.update({models: result.models ?? [], ...(profile ? {profile} : {})});
  }

  private async fetchBootstrap(): Promise<Bootstrap> {
    const response = await fetch("/api/v1/bootstrap", {
      cache: "no-store",
      credentials: "same-origin"
    });
    if (!response.ok) {
      throw new Error(`Bootstrap failed (${response.status})`);
    }
    return response.json() as Promise<Bootstrap>;
  }

  private async call<T>(
    route: WebRPCRoute,
    body: unknown,
    options: {
      idempotencyKey?: string;
      retryNetwork?: boolean;
      workspaceID?: string;
      signal?: AbortSignal;
    } = {}
  ): Promise<T> {
    const headers: Record<string, string> = {
      "Authorization": `Bearer ${this.token}`,
      "Content-Type": "application/json",
      "X-QCode-Request-ID": crypto.randomUUID()
    };
    const workspaceID = options.workspaceID ?? this.state.selectedWorkspaceID;
    if (workspaceID) {
      headers["X-QCode-Workspace-ID"] = workspaceID;
    }
    if (options.idempotencyKey) {
      headers["Idempotency-Key"] = options.idempotencyKey;
    }
    let response: Response;
    try {
      response = await fetch(`/api/v1/${route}`, {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        signal: options.signal
      });
    } catch (error) {
      if (!options.retryNetwork) throw error;
      response = await fetch(`/api/v1/${route}`, {
        method: "POST",
        headers,
        body: JSON.stringify(body)
      });
    }
    const envelope = (await response.json()) as Envelope<T>;
    if (!response.ok || envelope.problem) {
      throw new RuntimeProblem(
        envelope.problem ?? {
          version: 1,
          code: "internal",
          message: `Request failed (${response.status})`,
          retryable: false
        }
      );
    }
    if (envelope.result === undefined) {
      throw new Error(`Web API ${route} returned no result`);
    }
    return envelope.result;
  }

  private connect(): Promise<void> {
    const generation = ++this.generation;
    const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${window.location.host}/api/v1/events`);
    this.socket = socket;
    return new Promise<void>((resolve, reject) => {
      let connected = false;
      socket.addEventListener("open", () => {
        if (generation !== this.generation) return;
        socket.send(JSON.stringify({
          type: "authenticate",
          token: this.token,
          workspace_id: this.state.selectedWorkspaceID,
          cursor: this.cursor
        }));
      });
      socket.addEventListener("message", (message) => {
        if (generation !== this.generation) return;
        let frame: EventFrame;
        try {
          frame = decodeEventFrame(message.data);
        } catch (error) {
          this.resetProjection();
          this.update({
            phase: "desynchronized",
            socketConnected: false,
            problem: protocolProblem(error)
          });
          socket.close();
          if (!connected) reject(error);
          return;
        }
        if (frame.type === "hello") {
          connected = true;
          this.update({socketConnected: true, phase: "ready"});
          resolve();
          return;
        }
        if (!connected) {
          const error = new Error("Web event stream sent data before hello");
          this.resetProjection();
          this.update({
            phase: "desynchronized",
            socketConnected: false,
            problem: protocolProblem(error)
          });
          socket.close();
          reject(error);
          return;
        }
        if (frame.type === "desync") {
          this.resetProjection();
          this.update({
            phase: "desynchronized",
            socketConnected: false,
            problem: frame.problem
          });
          socket.close();
          return;
        }
        if (frame.type === "resync") {
          this.resetProjection();
          this.update({
            phase: "reconnecting",
            socketConnected: false,
            problem: frame.problem
          });
          socket.close();
          return;
        }
        if (frame.type === "watermark") {
          this.commitCursor(frame.sequence);
          return;
        }
        if (frame.type === "event" && frame.event) {
          this.applyEvent(frame.event, frame.session_id ?? "");
        }
      });
      socket.addEventListener("close", () => {
        if (generation !== this.generation) {
          if (!connected) reject(new Error("Web event stream was superseded"));
          return;
        }
        if (this.state.phase === "desynchronized") {
          return;
        }
        this.fail("Connection interrupted.");
        if (!connected) {
          reject(new Error("Web event stream closed before readiness"));
        }
        this.reconnectTimer = window.setTimeout(() => void this.start(), 700);
      });
      socket.addEventListener("error", () => {
        socket.close();
      });
    });
  }

  private resetProjection(): void {
    this.eventNotifier.cancel();
    this.pendingSelectedEvents = [];
    this.commitCursor(0, true);
    this.cancelProgressRefresh();
    this.selectionGeneration += 1;
    this.hydration = undefined;
    this.update({
      events: [],
      conversation: this.replaceConversation([]),
      queuedTurns: [],
      historyMoreBefore: false,
      hydratingSessionID: "",
      profile: undefined,
      tools: [],
      checkpoints: [],
      plan: undefined,
      agents: [],
      usage: undefined,
      trace: undefined,
      tracePhase: "idle",
      traceProblem: undefined,
      extensions: [],
      mergePlan: undefined,
      contextResources: []
    });
  }

  private applyEvent(event: RuntimeEvent, sessionID: string): void {
    if (event.sequence <= this.cursor) return;
    if (this.hydration) {
      if (!this.hydration.events.some(({event: value}) => value.sequence === event.sequence)) {
        this.hydration.events.push({event, sessionID});
      }
      return;
    }
    this.commitCursor(event.sequence);
    if (sessionID !== this.state.selectedSessionID) {
      if (sessionActivityEventKinds.has(event.kind)) {
        this.scheduleSessionRefresh();
      }
      return;
    }
    this.pendingSelectedEvents.push(event);
    if (immediateEventKinds.has(event.kind)) {
      this.eventNotifier.flushNow();
    } else {
      this.eventNotifier.schedule();
    }
    if (sessionActivityEventKinds.has(event.kind)) {
      this.scheduleSessionRefresh();
    }
    if (isTerminal(event.kind)) {
      this.scheduleSessionRefresh();
      void this.refreshUsage(sessionID);
      if (this.traceTurns.has(event.turn_id)) this.tracePending.add(event.turn_id);
      void this.refreshTrace(sessionID);
    }
    if (isTerminal(event.kind) || progressEventKinds.has(event.kind)) {
      void this.refreshProgress(sessionID);
    }
  }

  private flushSelectedEvents(): void {
    if (this.pendingSelectedEvents.length === 0) return;
    const pending = this.pendingSelectedEvents;
    this.pendingSelectedEvents = [];
    for (const event of pending) {
      this.conversationProjection.apply(event);
      if (event.kind === "turn.started" && event.turn_id) {
        this.traceTurns.add(event.turn_id);
        this.tracePending.add(event.turn_id);
      }
    }
    this.update({
      events: [...this.state.events, ...pending],
      conversation: this.conversationProjection.snapshot(),
      queuedTurns: projectTurnQueue(this.state.queuedTurns, pending)
    });
  }

  private replaceConversation(
    events: readonly RuntimeEvent[]
  ): ConversationSnapshot {
    this.conversationProjection = new ConversationProjection();
    this.conversationProjection.applyAll(events);
    if (events.length === 0) {
      this.traceTurns.clear();
      this.tracePending.clear();
    }
    for (const id of turnIDs(events)) {
      if (!this.traceTurns.has(id)) {
        this.traceTurns.add(id);
        this.tracePending.add(id);
      }
    }
    return this.conversationProjection.snapshot();
  }

  private requireQueuedTurn(queueID: string): QueuedTurn {
    const item = this.state.queuedTurns.find((value) => value.queue_id === queueID);
    if (!item) throw new Error("Queued turn is no longer available");
    return item;
  }

  private async refreshUsage(sessionID: string): Promise<void> {
    const generation = this.selectionGeneration;
    const result = await this.call<UsageQueryResult>("usage/query", {
      session_id: sessionID,
      include_children: true,
      limit: 100
    });
    if (
      generation === this.selectionGeneration &&
      sessionID === this.state.selectedSessionID
    ) {
      this.update({usage: result.rollup});
    }
  }

  private async refreshProgress(sessionID: string): Promise<void> {
    if (!sessionID || sessionID !== this.state.selectedSessionID) return;
    if (this.progressRequest) {
      this.progressRequest.dirty = true;
      return this.progressRequest.promise;
    }
    const generation = this.selectionGeneration;
    const workspaceID = this.state.selectedWorkspaceID;
    const request = {
      controller: new AbortController(),
      promise: Promise.resolve(),
      dirty: false
    };
    this.progressRequest = request;
    const current = () => this.progressRequest === request &&
      generation === this.selectionGeneration &&
      workspaceID === this.state.selectedWorkspaceID &&
      sessionID === this.state.selectedSessionID &&
      !request.controller.signal.aborted;
    const options = {workspaceID, signal: request.controller.signal};
    request.promise = (async () => {
      try {
        // Merge invalidations already queued in this task without a fixed delay.
        await Promise.resolve();
        while (current()) {
          request.dirty = false;
          const [plan, agents] = await Promise.allSettled([
            this.call<SessionPlanSnapshot>("plan/get", {session_id: sessionID}, options),
            this.call<AgentList>("agent/list", {session_id: sessionID, limit: 20}, options)
          ]);
          if (!current()) return;
          // Events received during the read invalidate both projections. Read
          // once more after they settle; never publish that older response.
          if (request.dirty) continue;
          const patch: Partial<RuntimeSnapshot> = {};
          if (plan.status === "fulfilled") {
            try {
              patch.plan = hydratePlanArtifact(plan.value.artifact);
            } catch {
              // Preserve the last valid plan if its document cannot be decoded.
            }
          }
          if (agents.status === "fulfilled") patch.agents = agents.value.agents ?? [];
          if (Object.keys(patch).length > 0) this.update(patch);
          if (!request.dirty) return;
        }
      } finally {
        if (this.progressRequest === request) {
          this.progressRequest = undefined;
        }
      }
    })();
    return request.promise;
  }

  private cancelProgressRefresh(): void {
    this.progressRequest?.controller.abort();
    this.progressRequest = undefined;
  }

  async refreshTrace(sessionID = this.state.selectedSessionID): Promise<void> {
    if (!sessionID || sessionID !== this.state.selectedSessionID) return;
    this.eventNotifier.flushNow();
    if (this.traceInFlight) {
      this.traceRefreshAgain = true;
      return this.traceInFlight;
    }
    const generation = this.selectionGeneration;
    const workspaceID = this.state.selectedWorkspaceID;
    const signal = this.selectionController?.signal;
    if (this.tracePending.size === 0) {
      this.update({tracePhase: "ready", traceProblem: undefined});
      return;
    }
    const current = () => generation === this.selectionGeneration &&
      sessionID === this.state.selectedSessionID && !signal?.aborted;
    const request = (async () => {
      do {
        this.traceRefreshAgain = false;
        const ids = [...this.tracePending];
        if (ids.length === 0 || !current()) return;
        this.update({tracePhase: "loading", traceProblem: undefined});
        try {
          const trace = await this.call<TraceSnapshot>("trace/query", {
            session_id: sessionID, turn_ids: ids,
            through_sequence: this.traceWatermark
          }, {workspaceID, signal});
          if (!current()) return;
          const turns = new Map((this.state.trace?.turns ?? [])
            .map((turn) => [turn.turn_id, turn]));
          for (const turn of trace.turns) {
            turns.set(turn.turn_id, turn);
            if (turn.complete) this.tracePending.delete(turn.turn_id);
          }
          this.update({
            trace: {...trace, turns: [...turns.values()]},
            tracePhase: "ready", traceProblem: undefined
          });
        } catch (error) {
          if (current()) this.update({
            tracePhase: "unavailable", traceProblem: errorMessage(error)
          });
          return;
        }
      } while (this.traceRefreshAgain);
    })();
    this.traceInFlight = request;
    try {
      await request;
    } finally {
      if (this.traceInFlight === request) {
        this.traceInFlight = undefined;
      }
    }
  }

  private scheduleSessionRefresh(): void {
    // The event socket is bound to the selected Workspace, including events
    // for Sessions that have not appeared in the catalog yet.
    if (this.state.selectedWorkspaceID) {
      this.sessionRefreshPending.add(this.state.selectedWorkspaceID);
    }
    this.queueSessionRefresh();
  }

  private queueSessionRefresh(): void {
    if (this.sessionRefreshQueued || this.sessionListRequest ||
        this.sessionRefreshPending.size === 0) return;
    this.sessionRefreshQueued = true;
    const generation = this.generation;
    queueMicrotask(() => {
      this.sessionRefreshQueued = false;
      if (generation !== this.generation) return;
      if (this.sessionListRequest || this.sessionRefreshPending.size === 0) return;
      const workspaceIDs = new Set(this.sessionRefreshPending);
      this.sessionRefreshPending.clear();
      void this.startSessionRefresh(
        undefined, false, this.state.includeArchived, workspaceIDs
      );
    });
  }

  private update(patch: Partial<RuntimeSnapshot>): void {
    this.state = Object.freeze({...this.state, ...patch});
    this.listeners.forEach((listener) => listener());
  }

  private workspaceIDForSession(sessionID: string): string {
    const ownerWorkspaceID = this.sessionWorkspaceIDs.get(sessionID);
    if (ownerWorkspaceID) return ownerWorkspaceID;
    const root = this.state.sessions.find(
      (session) => session.session_id === sessionID
    )?.workspace_root;
    return this.state.workspaces.find((workspace) => workspace.root === root)?.id ??
      this.state.selectedWorkspaceID;
  }

  private async refreshModelCatalog(): Promise<void> {
    const [providers, models] = await Promise.all([
      this.call<ProviderCatalog>("provider/list", {}),
      this.call<ModelCatalog>("model/list", {})
    ]);
    this.update({
      providers: providers.providers ?? [],
      models: models.models ?? []
    });
  }

  private protocolVersion: number = webProtocolVersion;
  private serverBuild = "";

  private async awaitWorkspaceReady(workspaceID: string): Promise<void> {
    for (let attempt = 0; attempt < 50; attempt += 1) {
      const catalog = await this.call<WorkspaceCatalog>(
        "workspace/list",
        {}
      ).catch(() => undefined);
      const workspace = catalog?.workspaces?.find(
        (entry) => entry.id === workspaceID
      );
      if (!workspace) throw new Error("Workspace is not registered");
      if (workspace.ready) {
        if (catalog?.workspaces) this.update({workspaces: catalog.workspaces});
        return;
      }
      if (workspace.problem) {
        throw new Error(workspace.problem);
      }
      // 仍在激活中：400ms 后重试，约 20s 上限。
      await new Promise((resolve) => window.setTimeout(resolve, 400));
    }
    throw new Error("Workspace Runtime is not ready");
  }

  private async restoreBrowserState(
    bootstrap: Bootstrap,
    workspaceID = bootstrap.workspace?.root_id ?? bootstrap.workspace_root ?? ""
  ): Promise<void> {
    this.protocolVersion = bootstrap.protocol_version;
    this.serverBuild = bootstrap.server_build || "unknown";
    const scope = [
      `v${bootstrap.protocol_version}`,
      this.serverBuild,
      workspaceID
    ].join(":");
    if (scope === this.storageScope) return;
    const restored = await this.storage.load(scope).catch(() => undefined);
    // Inputs can still arrive in the old Workspace while storage is loading.
    // Flush them under its scope before publishing the new selection and drafts.
    this.flushBrowserState();
    this.storageScope = scope;
    this.stored = {
      cursor: Math.max(0, restored?.cursor ?? 0),
      selectedSessionID: restored?.selectedSessionID ?? "",
      drafts: {...(restored?.drafts ?? {})},
      messageFeedback: {...(restored?.messageFeedback ?? {})}
    };
    this.cursor = Math.max(0, this.stored.cursor);
    this.update({
      selectedWorkspaceID: workspaceID,
      selectedSessionID: this.stored.selectedSessionID,
      events: [],
      conversation: this.replaceConversation([]),
      queuedTurns: [],
      historyMoreBefore: false,
      providers: [],
      models: [],
      profile: undefined,
      tools: [],
      checkpoints: [],
      plan: undefined,
      agents: [],
      usage: undefined,
      trace: undefined,
      tracePhase: "idle",
      traceProblem: undefined,
      extensions: [],
      mergePlan: undefined,
      contextResources: [],
      messageFeedback: {...(this.stored.messageFeedback ?? {})}
    });
  }

  private commitCursor(cursor: number, allowReset = false): void {
    const next = allowReset ? cursor : Math.max(this.cursor, cursor);
    if (next === this.cursor && next === this.stored.cursor) return;
    this.cursor = next;
    this.stored = {...this.stored, cursor: next};
    this.persistBrowserState();
  }

  private persistSelectedSession(sessionID: string): void {
    if (this.stored.selectedSessionID === sessionID) return;
    this.stored = {...this.stored, selectedSessionID: sessionID};
    this.persistBrowserState();
  }

  private persistBrowserState(): void {
    if (!this.storageScope) return;
    this.pendingStorage = {
      scope: this.storageScope,
      value: {
        cursor: this.stored.cursor,
        selectedSessionID: this.stored.selectedSessionID,
        drafts: {...this.stored.drafts},
        messageFeedback: {...(this.stored.messageFeedback ?? {})}
      }
    };
    if (this.storageTimer !== undefined) return;
    this.storageTimer = window.setTimeout(this.flushBrowserState, 100);
  }

  private readonly flushWhenHidden = (): void => {
    if (document.visibilityState === "hidden") this.flushBrowserState();
  };

  private readonly flushBrowserState = (): void => {
    if (this.storageTimer !== undefined) {
      window.clearTimeout(this.storageTimer);
      this.storageTimer = undefined;
    }
    const pending = this.pendingStorage;
    this.pendingStorage = undefined;
    if (!pending) return;
    this.storageWrite = this.storageWrite
      .catch(() => undefined)
      .then(() => this.storage.save(pending.scope, pending.value))
      .catch(() => undefined);
  };

  private fail(error: unknown): void {
    const problem =
      error instanceof RuntimeProblem
        ? error.problem
        : {
            version: 1,
            code: "internal",
            message: error instanceof Error ? error.message : String(error),
            retryable: true
          };
    this.update({phase: "failed", socketConnected: false, problem});
  }

  private requireSession(): string {
    if (this.state.hydratingSessionID) {
      throw new Error("Session is still loading");
    }
    if (!this.state.selectedSessionID) {
      throw new Error("No active session");
    }
    return this.state.selectedSessionID;
  }
}

export class RuntimeProblem extends Error {
  constructor(readonly problem: Problem) {
    super(problem.message);
  }
}

const eventKindSet = new Set<string>(webEventKinds);
const eventFrameTypes = new Set(["hello", "event", "watermark", "resync", "desync"]);

function decodeEventFrame(value: unknown): EventFrame {
  const decoded = JSON.parse(String(value)) as unknown;
  if (!decoded || typeof decoded !== "object") {
    throw new Error("Web event frame must be an object");
  }
  const frame = decoded as Partial<EventFrame>;
  if (frame.protocol_version !== webProtocolVersion) {
    throw new Error(`Unsupported Web protocol version ${String(frame.protocol_version)}`);
  }
  if (typeof frame.type !== "string" || !eventFrameTypes.has(frame.type)) {
    throw new Error(`Unknown Web event frame type ${String(frame.type)}`);
  }
  if (!Number.isSafeInteger(frame.sequence) || Number(frame.sequence) < 0) {
    throw new Error("Web event frame sequence is invalid");
  }
  if (frame.type === "event") {
    if (!frame.event || !eventKindSet.has(frame.event.kind) ||
        frame.event.sequence !== frame.sequence) {
      throw new Error("Web event frame contains an unknown or inconsistent event");
    }
  } else if (frame.event !== undefined) {
    throw new Error("Non-event Web frame contains an event payload");
  }
  return frame as EventFrame;
}

function protocolProblem(error: unknown): Problem {
  return {
    version: 1,
    code: "protocol_mismatch",
    message: error instanceof Error ? error.message : String(error),
    retryable: false
  };
}

function turnIDs(events: readonly RuntimeEvent[]): string[] {
  return [...new Set(events
    .filter((event) => event.kind === "turn.started")
    .map((event) => event.turn_id)
    .filter(Boolean))];
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function workspaceCatalogFromBootstrap(bootstrap: Bootstrap): WorkspaceCatalog {
  if (bootstrap.workspace_catalog?.version) {
    return bootstrap.workspace_catalog;
  }
  const workspace = bootstrap.workspace;
  if (!workspace || !bootstrap.workspace_root) {
    return {version: 1, workspaces: []};
  }
  return {
    version: 1,
    workspaces: [{
      id: workspace.root_id,
      root: bootstrap.workspace_root,
      label: bootstrap.workspace_root.split(/[\\/]/).filter(Boolean).at(-1) ||
        bootstrap.workspace_root,
      ready: bootstrap.ready,
      removable: true,
      session_count: 0
    }]
  };
}

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value)
  );
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

export function isTerminal(kind: string): boolean {
  return kind === "turn.completed" || kind === "turn.failed" || kind === "turn.canceled";
}
