import {afterEach, beforeEach, describe, expect, it, vi} from "vitest";
import type {AgentPreset} from "../protocol";
import {RuntimeClient} from "./client";
import type {
  BrowserProjectionState,
  BrowserStorage
} from "./storage";

class FakeWebSocket {
  static readonly OPEN = 1;
  static instances: FakeWebSocket[] = [];
  readonly readyState = FakeWebSocket.OPEN;
  readonly sent: string[] = [];
  private listeners = new Map<string, Array<(event: MessageEvent | Event) => void>>();

  constructor() {
    FakeWebSocket.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent | Event) => void): void {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  close(): void {}

  send(value: string): void {
    this.sent.push(value);
  }

  emit(type: string, value?: unknown): void {
    const event =
      type === "message"
        ? new MessageEvent("message", {data: JSON.stringify(value)})
        : new Event(type);
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event);
    }
  }
}

async function startClient(client: RuntimeClient): Promise<FakeWebSocket> {
  const previousCount = FakeWebSocket.instances.length;
  const started = client.start();
  await vi.waitFor(() => {
    expect(FakeWebSocket.instances.length).toBeGreaterThan(previousCount);
  });
  const socket = FakeWebSocket.instances[previousCount];
  if (!socket) throw new Error("missing WebSocket");
  socket.emit("open");
  const authentication = JSON.parse(socket.sent.at(-1) ?? "{}") as {cursor?: number};
  socket.emit("message", {
    type: "hello",
    protocol_version: 1,
    sequence: authentication.cursor ?? 0
  });
  await started;
  return socket;
}

function deferProgressReads() {
  type Read = {
    resolve: (response: Response) => void;
    signal: AbortSignal | null | undefined;
    sessionID: string;
    workspaceID: string | null;
  };
  const plans: Read[] = [];
  const agents: Read[] = [];
  const originalFetch = globalThis.fetch;
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const route = String(input);
    const pending = route.endsWith("/plan/get") ? plans
      : route.endsWith("/agent/list") ? agents : undefined;
    if (!pending) return originalFetch(input, init);
    return new Promise<Response>((resolve) => pending.push({
      resolve,
      signal: init?.signal,
      sessionID: JSON.parse(String(init?.body)).session_id,
      workspaceID: new Headers(init?.headers).get("X-QCode-Workspace-ID")
    }));
  }));
  return {plans, agents};
}

function deferSessionReads() {
  const reads: Array<{
    resolve: (response: Response) => void;
    signal: AbortSignal | null | undefined;
    query: string;
    workspaceID: string | null;
  }> = [];
  const originalFetch = globalThis.fetch;
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    if (!String(input).endsWith("/session/list")) return originalFetch(input, init);
    return new Promise<Response>((resolve) => reads.push({
      resolve, signal: init?.signal,
      query: JSON.parse(String(init?.body)).query,
      workspaceID: new Headers(init?.headers).get("X-QCode-Workspace-ID")
    }));
  }));
  return {reads, originalFetch};
}

describe("RuntimeClient", () => {
  const requests: Array<{
    route: string;
    body: Record<string, unknown>;
    headers: Headers;
  }> = [];
  let bootstrapToken = "token";
  let snapshotSequence = 0;
  let snapshotGate: Promise<void> | undefined;
  let profileGate: Promise<void> | undefined;
  let traceGate: Promise<void> | undefined;
  let traceComplete = true;
  let failNextCreate = false;
  let createdSession = false;
  let failToolCatalog = false;
  let currentProvider = "fixture";
  let currentModel = "fixture";
  let currentApproval = "suggest";
  let snapshotEvents: ReturnType<typeof runtimeEvent>[] | null = null;
  let snapshotTruncatedBefore = 0;
  let earlierEvents: ReturnType<typeof runtimeEvent>[] = [];
  let historyGate: Promise<void> | undefined;
  let moreBefore = false;
  let presets: AgentPreset[] = [];
  let presetRevision = 0;
  let setupRequired = false;
  let multipleWorkspaces = false;
  let emptyPrimaryWorkspace = false;
  let activePlan = false;

  beforeEach(() => {
    requests.length = 0;
    FakeWebSocket.instances.length = 0;
    bootstrapToken = "token";
    snapshotSequence = 0;
    snapshotGate = undefined;
    profileGate = undefined;
    traceGate = undefined;
    traceComplete = true;
    failNextCreate = false;
    createdSession = false;
    failToolCatalog = false;
    currentProvider = "fixture";
    currentModel = "fixture";
    currentApproval = "suggest";
    snapshotEvents = null;
    snapshotTruncatedBefore = 0;
    earlierEvents = [];
    historyGate = undefined;
    moreBefore = false;
    presets = [];
    presetRevision = 0;
    setupRequired = false;
    multipleWorkspaces = false;
    emptyPrimaryWorkspace = false;
    activePlan = false;
    window.history.replaceState(null, "", "/?workspace=workspace-id");
    vi.stubGlobal("WebSocket", FakeWebSocket);
    vi.stubGlobal("crypto", {
      randomUUID: () => "request-id",
      subtle: {
        digest: vi.fn(async () => new Uint8Array(32).fill(0xab).buffer)
      }
    });
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const route = String(input);
      const workspaces = [{
        id: "workspace-id",
        root: "/workspace",
        label: "workspace",
        ready: true,
        removable: true,
        session_count: emptyPrimaryWorkspace ? 0 : 1
      }];
      if (multipleWorkspaces) {
        workspaces.push({
          id: "workspace-b-id",
          root: "/workspace-b",
          label: "workspace-b",
          ready: true,
          removable: true,
          session_count: 1
        });
      }
      if (route.endsWith("/bootstrap")) {
        if (setupRequired) {
          return response({
            protocol_version: 1,
            server_build: "build",
            token: bootstrapToken,
            ready: false,
            draining: false,
            setup_required: true,
            workspace_root: "/workspace"
          });
        }
        return response({
          protocol_version: 1,
          server_build: "build",
          token: bootstrapToken,
          ready: true,
          draining: false,
          workspace_root: "/workspace",
          workspace: {
            version: 1,
            root_id: "workspace-id",
            editor_uri: "file:///workspace",
            runtime_path: "/workspace"
          },
          workspace_catalog: {
            version: 1,
            workspaces
          }
        });
      }
      if (route.endsWith("/workspace/list")) {
        return envelope({version: 1, workspaces});
      }
      if (route.includes("/api/v1/content/")) {
        requests.push({
          route,
          body: {},
          headers: new Headers(init?.headers)
        });
        return new Response("package main\n", {
          status: 200,
          headers: {"Content-Type": "text/plain; charset=utf-8"}
        });
      }
      const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
      requests.push({route, body, headers: new Headers(init?.headers)});
      if (route.endsWith("/setup/apply")) {
        setupRequired = false;
        return envelope({ready: true});
      }
      if (route.endsWith("/provider/list")) {
        return envelope({
          version: 1,
          providers: [
            {
              id: "fixture",
              display_name: "Fixture",
              selected: true,
              availability: "available"
            },
            {
              id: "offline",
              display_name: "Offline",
              selected: false,
              availability: "unavailable",
              reason: "Credential missing"
            }
          ]
        });
      }
      if (route.endsWith("/model/list")) {
        return envelope({
          version: 1,
          models: [
            {
              provider: "fixture",
              id: "fixture",
              selected: true,
              capabilities: modelCapabilities("Fixture")
            },
            {
              provider: "fixture",
              id: "reasoner",
              selected: false,
              capabilities: {
                ...modelCapabilities("Reasoner"),
                reasoning: true,
                reasoning_efforts: ["low", "high"]
              }
            }
          ]
        });
      }
      if (route.endsWith("/session/create")) {
        if (failNextCreate) {
          failNextCreate = false;
          throw new TypeError("connection reset");
        }
        createdSession = true;
        return envelope({
          session_id: "session-new",
          thread_id: "thread-new",
          workspace_root: "/workspace",
          provider: "fixture",
          model: "fixture",
          isolation: body.isolation
        });
      }
      if (route.endsWith("/session/delete")) {
        return envelope({
          version: 1,
          session_id: body.session_id,
          thread_id: "thread",
          deleted_at: "2026-01-01T00:00:00Z"
        });
      }
      if (route.endsWith("/session/update")) {
        return envelope({
          session: {
            version: 1,
            revision: 2,
            session_id: body.session_id,
            thread_id: body.session_id === "session-b" ? "thread-b" : "thread",
            title: "Updated",
            status: "idle",
            pinned: true,
            archived: false,
            isolation: "shared",
            workspace_root: body.session_id === "session-b"
              ? "/workspace-b"
              : "/workspace",
            workspace_label: body.session_id === "session-b"
              ? "workspace-b"
              : "workspace",
            pending_approvals: 0,
            pending_inputs: 0,
            checkpoint_count: 0,
            changed_files: 0,
            total_tokens: 0,
            cost_microunits: 0,
            cost_known: true,
            created_at: "2026-01-01T00:00:00Z",
            updated_at: "2026-01-01T00:00:00Z"
          }
        });
      }
      if (route.endsWith("/workspace/git-switch")) {
        return envelope({
          repository: true,
          branch: body.branch,
          branches: ["feature", "main"],
          dirty: false
        });
      }
      if (route.endsWith("/session/list")) {
        const workspaceID = new Headers(init?.headers)
          .get("X-QCode-Workspace-ID");
        const secondary = workspaceID === "workspace-b-id";
        if (!secondary && emptyPrimaryWorkspace && !createdSession) {
          return envelope({version: 1, sessions: []});
        }
        const sessions = [{
          version: 1,
          revision: 1,
          session_id: secondary ? "session-b" : "session",
          thread_id: secondary ? "thread-b" : "thread",
          title: secondary ? "Chat B" : "Chat",
          status: "idle",
          pinned: false,
          archived: false,
          isolation: "shared",
          workspace_root: secondary ? "/workspace-b" : "/workspace",
          workspace_label: secondary ? "workspace-b" : "workspace",
          latest_turn_id: "turn",
          latest_sequence: 0,
          pending_approvals: 0,
          pending_inputs: 0,
          checkpoint_count: 0,
          changed_files: 0,
          total_tokens: 0,
          cost_microunits: 0,
          cost_known: true,
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z"
        }];
        if (createdSession) {
          sessions.push({
            ...sessions[0],
            session_id: "session-new",
            thread_id: "thread-new",
            title: "New Chat"
          });
        }
        return envelope({
          version: 1,
          sessions
        });
      }
      if (route.endsWith("/session/activate")) {
        return envelope({
          session_id: body.session_id,
          thread_id: body.session_id === "session-b" ? "thread-b" : "thread",
          workspace_root: body.session_id === "session-b"
            ? "/workspace-b"
            : "/workspace",
          provider: "fixture",
          model: "fixture",
          isolation: "shared"
        });
      }
      if (route.endsWith("/session/snapshot")) {
        await snapshotGate;
        return envelope({
          version: 1,
          session_id: body.session_id,
          thread_id: body.session_id === "session-b" ? "thread-b" : "thread",
          session_revision: 1,
          through_sequence: snapshotSequence,
          events: snapshotEvents,
          history_truncated_before: snapshotTruncatedBefore
        });
      }
      if (route.endsWith("/session/history")) {
        const page = {
          session_id: "session",
          events: earlierEvents,
          next_sequence: earlierEvents.at(-1)?.sequence ?? 0,
          more: false,
          previous_sequence: earlierEvents[0]?.sequence ?? 0,
          more_before: moreBefore
        };
        await historyGate;
        return envelope(page);
      }
      if (route.endsWith("/profile/get")) {
        await profileGate;
        return envelope({
          profile: {
            version: 1,
            revision: 1,
            mode: "act",
            provider: currentProvider,
            model: currentModel,
            approval_posture: currentApproval,
            execution_target: "local",
            max_steps: 0,
            prompt_cache_revision: 1
          },
          capabilities: {
            provider: currentProvider,
            model: currentModel,
            mutable_fields: ["mode", "provider", "model"],
            model_capabilities: modelCapabilities(
              currentModel === "reasoner" ? "Reasoner" : "Fixture"
            )
          }
        });
      }
      if (route.endsWith("/profile/update")) {
        const patch = body.patch as Record<string, unknown>;
        currentProvider = String(patch.provider ?? currentProvider);
        currentModel = String(patch.model ?? currentModel);
        currentApproval = String(patch.approval_posture ?? currentApproval);
        return envelope({
          profile: {
            version: 1,
            revision: 2,
            mode: "act",
            provider: currentProvider,
            model: currentModel,
            approval_posture: currentApproval,
            execution_target: "local",
            max_steps: 0
          },
          prompt_cache_reset: true
        });
      }
      if (route.endsWith("/tool/catalog")) {
        if (failToolCatalog) {
          return envelopeProblem("unavailable", "catalog unavailable", true);
        }
        return envelope({
          version: 1,
          catalog_id: "catalog",
          generation: 1,
          digest: "digest",
          tools: null
        });
      }
      if (route.endsWith("/checkpoint/list")) {
        return envelope({version: 1, session_id: "session", checkpoints: null});
      }
      if (route.endsWith("/plan/get")) {
        return envelope(activePlan ? {
          version: 2,
          artifact: {
            version: 2,
            id: "plan-id",
            session_id: "session",
            thread_id: "thread",
            turn_id: "plan-turn",
            cursor: 1,
            status: "ready",
            body: `{"version":1,"revision":1,"steps":[]}`,
            document: {version: 1, revision: 1, steps: []},
            profile_revision: 1,
            can_implement: true,
            can_autopilot: true,
            created_at: "2026-01-01T00:00:00Z"
          }
        } : {version: 1});
      }
      if (route.endsWith("/agent/list")) {
        return envelope({agents: []});
      }
      if (route.endsWith("/turn/queue")) {
        return envelope({version: 1, items: []});
      }
      if (route.endsWith("/trace/query")) {
        const complete = traceComplete;
        await traceGate;
        return envelope({
          version: 1,
          session_id: body.session_id,
          through_sequence: body.through_sequence,
          turns: (body.turn_ids as string[]).map((turnID) => ({
            turn_id: turnID,
            complete,
            status: "ok",
            spans: []
          }))
        });
      }
      if (route.endsWith("/usage/query")) {
        return envelope({
          usage: [],
          rollup: {
            turns: 0,
            calls: 0,
            total_tokens: 0,
            cost_microunits: 0,
            cost_known: true
          }
        });
      }
      if (route.endsWith("/extension/list")) {
        return envelope({revision: 1, extensions: []});
      }
      if (route.endsWith("/extension/control")) {
        return envelope({
          operation_id: body.id,
          revision: 2,
          detail: {name: body.name, action: body.action},
          receipt: {
            operation_id: body.id,
            action: body.action,
            kind: body.kind,
            name: body.name,
            status: "committed",
            digest: "f".repeat(64),
            revision: 2,
            occurred_at: "2026-01-01T00:00:00Z"
          }
        });
      }
      if (route.endsWith("/agent-preset/list")) {
        return envelope({
          version: 1,
          revision: presetRevision,
          presets
        });
      }
      if (route.endsWith("/agent-preset/save")) {
        const previous = presets.find((preset) => preset.id === body.id);
        presetRevision += 1;
        const preset: AgentPreset = {
          version: 1,
          id: String(body.id),
          revision: (previous?.revision ?? 0) + 1,
          name: String(body.name),
          description: String(body.description ?? ""),
          scope: "workspace",
          profile: body.profile as AgentPreset["profile"],
          created_at: previous?.created_at ?? "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z"
        };
        presets = [...presets.filter((value) => value.id !== preset.id), preset];
        return envelope({version: 1, revision: presetRevision, preset});
      }
      if (route.endsWith("/agent-preset/delete")) {
        presets = presets.filter((preset) => preset.id !== body.id);
        presetRevision += 1;
        return envelope({
          version: 1,
          revision: presetRevision,
          deleted_id: body.id
        });
      }
      if (route.endsWith("/agent-preset/apply")) {
        return envelope({
          version: 1,
          preset_id: body.preset_id,
          profile_update: {
            profile: {
              version: 1,
              revision: 2,
              mode: "plan",
              provider: currentProvider,
              model: currentModel,
              approval_posture: "suggest",
              execution_target: "local",
              max_steps: 16,
              prompt_cache_revision: 2
            },
            prompt_cache_reset: true,
            reset_reason: "mode"
          },
          restart_required: false
        });
      }
      if (route.endsWith("/workspace/resource")) {
        return envelope({
          path: "src/main.go",
          uri: "file:///workspace/src/main.go",
          document_version: 1,
          content_handle: "signed-content-handle",
          content: "package main\n",
          digest: "a".repeat(64),
          bytes: 13
        });
      }
      if (route.endsWith("/workspace/select-directory")) {
        return envelope({path: "/workspace/selected"});
      }
      if (route.endsWith("/workspace/remove")) {
        const workspaces = [{
          id: "workspace-id",
          root: "/workspace",
          label: "workspace",
          ready: true,
          removable: true,
          session_count: 1
        }];
        if (multipleWorkspaces) {
          workspaces.push({
            id: "workspace-b-id",
            root: "/workspace-b",
            label: "workspace-b",
            ready: true,
            removable: true,
            session_count: 1
          });
        }
        return envelope({
          version: 1,
          workspaces: workspaces.filter(
            (workspace) => workspace.id !== body.workspace_id
          )
        });
      }
      if (route.endsWith("/workspace/image")) {
        return envelope({
          path: "diagram.png",
          uri: "file:///workspace/diagram.png",
          document_version: 1,
          content_handle: "signed-image-handle",
          digest: "c".repeat(64),
          bytes: 16,
          media_type: "image/png",
          label: "diagram.png"
        });
      }
      if (route.endsWith("/workspace/symbols")) {
        return envelope({
          query: "main",
          status: "ready",
          symbols: [{
            path: "src/main.go",
            name: "main",
            kind: "function",
            line: 3,
            uri: "file:///workspace/src/main.go",
            document_version: 1,
            digest: "a".repeat(64),
            range: {
              start: {line: 2, character: 0},
              end: {line: 2, character: 14}
            },
            selection_range: {
              start: {line: 2, character: 5},
              end: {line: 2, character: 9}
            }
          }]
        });
      }
      if (route.endsWith("/workspace/diagnostics")) {
        return envelope({
          session_id: "session",
          thread_id: "thread",
          diagnostics: [{
            call_id: "call-1",
            tool: "exec_command",
            status: "failed",
            context: {
              kind: "diagnostics",
              source: "code_action",
              uri: "file:///workspace/src/main.go",
              path: "src/main.go",
              document_version: 1,
              digest: "a".repeat(64),
              diagnostics: [{
                range: {
                  start: {line: 2, character: 0},
                  end: {line: 2, character: 4}
                },
                severity: "error",
                message: "broken",
                source: "fixture"
              }],
              explicit: true
            }
          }]
        });
      }
      if (route.endsWith("/workspace/diff")) {
        return envelope({
          session_id: "session",
          thread_id: "thread",
          diff: "diff --git a/main.go b/main.go\n",
          digest: "b".repeat(64)
        });
      }
      if (route.endsWith("/workspace/git-status")) {
        return envelope({repository: true, branch: "main", branches: ["main"], files: [], remotes: []});
      }
      if (route.endsWith("/workspace/git-diff")) {
        return envelope({path: body.path, staged: body.staged, diff: "+changed"});
      }
      if (route.endsWith("/workspace/git-action")) {
        return envelope({action: body.action, commit_hash: "new-commit", completed: ["git_commit"]});
      }
      if (route.endsWith("/operation/submit")) {
        return envelope({
          operation_id: "operation",
          kind: "turn.start",
          thread_id: "thread",
          turn_id: "turn",
          item_id: "item",
          accepted: true
        });
      }
      if (route.endsWith("/turn/recover")) {
        return envelope({
          operation_id: "recovery-operation",
          kind: "turn.start",
          thread_id: "thread",
          turn_id: "recovered-turn",
          item_id: "recovered-item",
          accepted: true
        });
      }
      throw new Error(`unexpected route ${route}`);
    }));
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

    it("waits in setup state and activates only after explicit selection", async () => {
      setupRequired = true;
      const client = new RuntimeClient();
      await client.start();

      expect(client.getSnapshot().phase).toBe("setup");
      expect(FakeWebSocket.instances).toHaveLength(0);

      const completed = client.completeSetup({
        model: "deepseek-chat",
        api_key: "sk-test",
        base_url: "https://api.deepseek.com/v1",
        protocol: "openai_chat"
      });
      await vi.waitFor(() => {
        expect(FakeWebSocket.instances).toHaveLength(1);
      });
      const socket = FakeWebSocket.instances[0]!;
      socket.emit("open");
      socket.emit("message", {
        type: "hello",
        protocol_version: 1,
        sequence: 0
      });
      await completed;

      const setup = requests.find((request) => request.route.endsWith("/setup/apply"));
      expect(setup?.body).toEqual({
        model: "deepseek-chat",
        api_key: "sk-test",
        base_url: "https://api.deepseek.com/v1",
        protocol: "openai_chat"
      });
      expect(setup?.headers.get("Idempotency-Key")).toBe("request-id");
      expect(client.getSnapshot().phase).toBe("ready");
      expect(requests.some((request) =>
        request.route.endsWith("/session/create")
      )).toBe(false);
      client.stop();
    });

  it("normalizes empty collections and submits act prompts as answers", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    expect(client.getSnapshot().events).toEqual([]);
    expect(client.getSnapshot().providers.map((provider) => provider.id)).toEqual([
      "fixture",
      "offline"
    ]);
    expect(client.getSnapshot().models.map((model) => model.id)).toEqual([
      "fixture",
      "reasoner"
    ]);
    expect(client.getSnapshot().tools).toEqual([]);
    expect(client.getSnapshot().checkpoints).toEqual([]);

    const sessionListsBefore = requests.filter(
      (request) => request.route.endsWith("/session/list")
    ).length;
    await client.submitPrompt("say hello");
    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      kind: "turn.start",
      payload: {
        prompt: "say hello",
        display_prompt: "say hello",
        intent: "answer"
      }
    });
    expect(requests.filter(
      (request) => request.route.endsWith("/session/list")
    )).toHaveLength(sessionListsBefore + 1);
    client.stop();
  });

  it("submits steering against the active turn without starting another turn", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.steer("turn-active", "  focus on the failing test  ");

    const submit = requests.find((request) =>
      request.route.endsWith("/operation/submit")
    );
    expect(submit?.body).toMatchObject({
      kind: "turn.steer",
      payload: {
        turn_id: "turn-active",
        prompt: "focus on the failing test"
      }
    });
    client.stop();
  });

  it("uses the resumable interruption reason when stopping a turn", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.cancel("turn-active");

    expect(requests.find((request) =>
      request.route.endsWith("/operation/submit") &&
      request.body.kind === "turn.cancel"
    )?.body).toMatchObject({
      kind: "turn.cancel",
      payload: {
        turn_id: "turn-active",
        reason: "user_interrupted"
      }
    });
    client.stop();
  });

  it("submits recovery input as a new user prompt", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.recoverTurn("turn-paused", "continue", "Fix all five P2 issues");

    const recovery = requests.find((request) =>
      request.route.endsWith("/turn/recover")
    );
    expect(recovery?.body).toMatchObject({
      action: "continue",
      source_turn_id: "turn-paused",
      prompt: "Fix all five P2 issues"
    });
    expect(recovery?.body).not.toHaveProperty("guidance");
    client.stop();
  });

  it("submits and manages Runtime-owned queued turns", async () => {
    const client = new RuntimeClient();
    const socket = await startClient(client);

    await client.enqueue("turn-active", "  follow up  ");
    expect(requests.find((request) =>
      request.route.endsWith("/operation/submit") &&
      request.body.kind === "turn.enqueue"
    )?.body).toMatchObject({
      kind: "turn.enqueue",
      payload: {
        turn_id: "turn-active",
        prompt: "follow up",
        display_prompt: "follow up"
      }
    });

    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 1,
      event: runtimeEvent(1, "turn.queued", {
        queue_id: "queue-1",
        prompt: "follow up"
      })
    });
    await vi.waitFor(() => {
      expect(client.getSnapshot().queuedTurns).toHaveLength(1);
    });

    await client.updateQueuedTurn("queue-1", "revised");
    await client.promoteQueuedTurn("queue-1", "turn-active");
    await client.removeQueuedTurn("queue-1");
    const kinds = requests
      .filter((request) => request.route.endsWith("/operation/submit"))
      .map((request) => request.body.kind);
    expect(kinds).toEqual([
      "turn.enqueue",
      "turn.queue.update",
      "turn.queue.promote",
      "turn.queue.remove"
    ]);
    client.stop();
  });

  it("refreshes authoritative capabilities after changing the model", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.updateProfile({model: "reasoner"});

    expect(client.getSnapshot().profile).toMatchObject({
      profile: {provider: "fixture", model: "reasoner"},
      capabilities: {
        provider: "fixture",
        model: "reasoner",
        model_capabilities: {display_name: "Reasoner"}
      }
    });
    expect(
      requests.filter((request) => request.route.endsWith("/profile/get"))
    ).toHaveLength(2);
    client.stop();
  });

  it("keeps the selected session usable when an auxiliary query fails", async () => {
    failToolCatalog = true;
    const client = new RuntimeClient();
    await startClient(client);

    expect(client.getSnapshot()).toMatchObject({
      phase: "ready",
      selectedSessionID: "session",
      tools: []
    });
    client.stop();
  });

  it("streams drafts of a live turn below hydrated conversation turns", async () => {
    snapshotSequence = 3;
    snapshotEvents = [
      runtimeEvent(1, "turn.started", {display_prompt: "first question"}),
      runtimeEvent(2, "output.delta", {text: "first answer"}),
      runtimeEvent(3, "turn.completed", {text: "first answer"})
    ];
    const client = new RuntimeClient();
    const socket = await startClient(client);

    const frame = (sequence: number, turnID: string, kind: string, data: Record<string, unknown> = {}) =>
      socket.emit("message", {
        type: "event",
        protocol_version: 1,
        session_id: "session",
        sequence,
        event: {...runtimeEvent(sequence, kind, data), turn_id: turnID, id: `${turnID}-event-${sequence}`}
      });
    frame(4, "turn2", "turn.started", {display_prompt: "second question"});
    frame(5, "turn2", "output.draft", {text: "streaming ", sample_id: "s1"});
    frame(6, "turn2", "output.draft", {text: "draft", sample_id: "s1"});
    await vi.waitFor(() => {
      expect(client.getSnapshot().events.at(-1)?.sequence).toBe(6);
    });
    const conversation = client.getSnapshot().conversation;
    const rendered = conversation.order.flatMap((id) => {
      const node = conversation.nodes.get(id);
      return node ? [`${node.kind}:${node.id}`] : [];
    });
    expect(rendered).toEqual([
      "user:event-1",
      "assistant:output-turn",
      "user:turn2-event-4",
      "assistant:output-turn2"
    ]);
    client.stop();
  });

  it("queries trace timing against the hydrated event watermark", async () => {
    snapshotSequence = 3;
    snapshotEvents = [
      runtimeEvent(1, "turn.started"),
      runtimeEvent(2, "turn.completed"),
      {
        ...runtimeEvent(3, "agent.status"),
        thread_id: "thread_external",
        turn_id: "turn_external",
        data: {agent_id: "agent-1", status: "running"}
      }
    ];
    const client = new RuntimeClient();
    await startClient(client);

    expect(requests.find((request) => request.route.endsWith("/trace/query"))?.body)
      .toEqual({
        session_id: "session",
        turn_ids: ["turn"],
        through_sequence: 3
      });
    expect(client.getSnapshot().tracePhase).toBe("ready");
    client.stop();
  });

  it("keeps live trace queries on the last acknowledged watermark", async () => {
    traceComplete = false;
    snapshotSequence = 2;
    snapshotEvents = [
      runtimeEvent(1, "turn.started"),
      runtimeEvent(2, "turn.completed")
    ];
    const client = new RuntimeClient();
    const socket = await startClient(client);

    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 3,
      event: runtimeEvent(3, "tool.start")
    });
    await vi.waitFor(() => {
      expect(client.getSnapshot().events.at(-1)?.sequence).toBe(3);
    });
    await client.refreshTrace();

    const traceRequests = requests.filter(
      (request) => request.route.endsWith("/trace/query")
    );
    expect(traceRequests.at(-1)?.body).toMatchObject({
      session_id: "session",
      through_sequence: 2
    });
    client.stop();
  });

  it("publishes history and independent details before slow controls and Trace", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    snapshotSequence = 2;
    snapshotEvents = [runtimeEvent(1, "turn.started"), runtimeEvent(2, "turn.completed")];
    let releaseProfile = (): void => {};
    let releaseTrace = (): void => {};
    profileGate = new Promise<void>((resolve) => { releaseProfile = resolve; });
    traceGate = new Promise<void>((resolve) => { releaseTrace = resolve; });
    const selecting = client.selectSession("session");
    await vi.waitFor(() => {
      expect(client.getSnapshot().events).toHaveLength(2);
      expect(client.getSnapshot().usage).toBeDefined();
    });
    expect(client.getSnapshot().hydratingSessionID).toBe("session");
    await expect(client.submitPrompt("too early")).rejects.toThrow("Session is still loading");
    releaseProfile();
    await vi.waitFor(() => expect(client.getSnapshot().hydratingSessionID).toBe(""));
    expect(client.getSnapshot().tracePhase).toBe("loading");
    await client.submitPrompt("controls are ready");
    releaseTrace();
    await selecting;
    client.stop();
  });

  it("discards auxiliary responses from an earlier selection of the same Session", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    snapshotSequence = 2;
    snapshotEvents = [runtimeEvent(1, "turn.started"), runtimeEvent(2, "turn.completed")];
    traceComplete = false;
    let release = (): void => {};
    traceGate = new Promise<void>((resolve) => { release = resolve; });
    const old = client.selectSession("session");
    await vi.waitFor(() => expect(requests.filter((r) => r.route.endsWith("/trace/query"))).toHaveLength(1));
    traceGate = undefined;
    traceComplete = true;
    await client.selectSession("session");
    release();
    await old;
    expect(client.getSnapshot().trace?.turns[0]?.complete).toBe(true);
    client.stop();
  });

  it("coalesces progress events and never publishes a read superseded while in flight", async () => {
    activePlan = true;
    const client = new RuntimeClient();
    const socket = await startClient(client);
    const initialPlan = client.getSnapshot().plan!;
    const reads = deferProgressReads();
    const revisions: number[] = [];
    const unsubscribe = client.subscribe(() => {
      revisions.push(client.getSnapshot().plan?.document?.revision ?? 0);
    });
    const emit = (sequence: number, kind: string) => socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "session", sequence,
      event: runtimeEvent(sequence, kind)
    });
    emit(1, "plan.delta");
    await vi.waitFor(() => expect(reads.plans).toHaveLength(1));
    for (let sequence = 2; sequence <= 8; sequence++) emit(sequence, "agent.status");
    emit(9, "turn.completed");
    expect(reads.plans).toHaveLength(1);
    expect(reads.agents).toHaveLength(1);
    reads.plans[0].resolve(envelope({
      version: 2, artifact: {...initialPlan, document: {...initialPlan.document, revision: 2}}
    }));
    reads.agents[0].resolve(envelope({agents: [{agent_id: "agent", status: "running"}]}));
    await vi.waitFor(() => expect(reads.plans).toHaveLength(2));
    expect(revisions).not.toContain(2);
    expect(client.getSnapshot().agents).toEqual([]);
    reads.plans[1].resolve(envelope({
      version: 2, artifact: {...initialPlan, document: {...initialPlan.document, revision: 3}}
    }));
    reads.agents[1].resolve(envelope({agents: [{agent_id: "agent", status: "completed"}]}));
    await vi.waitFor(() => {
      expect(client.getSnapshot().plan?.document?.revision).toBe(3);
      expect(client.getSnapshot().agents[0]?.status).toBe("completed");
    });
    expect(reads.plans).toHaveLength(2);
    expect(reads.agents).toHaveLength(2);
    unsubscribe();
    client.stop();
  });

  it("shares progress hydration with live refresh and retains successful data after a partial failure", async () => {
    activePlan = true;
    const client = new RuntimeClient();
    const socket = await startClient(client);
    const initialPlan = client.getSnapshot().plan!;
    const reads = deferProgressReads();
    const selecting = client.selectSession("session");
    await vi.waitFor(() => expect(reads.plans).toHaveLength(1));
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "session", sequence: 1,
      event: runtimeEvent(1, "agent.status")
    });
    expect(reads.plans).toHaveLength(1);
    reads.plans[0].resolve(envelope({version: 1}));
    reads.agents[0].resolve(envelope({agents: [{agent_id: "agent", status: "running"}]}));
    await vi.waitFor(() => expect(reads.plans).toHaveLength(2));
    reads.plans[1].resolve(envelopeProblem("unavailable", "plan unavailable", true));
    reads.agents[1].resolve(envelope({agents: [{agent_id: "agent", status: "completed"}]}));
    await selecting;
    expect(client.getSnapshot().agents[0]?.status).toBe("completed");
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "session", sequence: 2,
      event: runtimeEvent(2, "plan.delta")
    });
    await vi.waitFor(() => expect(reads.plans).toHaveLength(3));
    reads.plans[2].resolve(envelope({
      version: 2, artifact: {...initialPlan, document: {...initialPlan.document, revision: 2}}
    }));
    reads.agents[2].resolve(envelopeProblem("unavailable", "agents unavailable", true));
    await vi.waitFor(() => expect(client.getSnapshot().plan?.document?.revision).toBe(2));
    expect(client.getSnapshot().agents[0]?.status).toBe("completed");
    client.stop();
  });

  it.each(["session", "workspace"])("cancels old progress on %s selection and ignores late responses", async (scope) => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);
    const reads = deferProgressReads();
    const oldSelection = client.selectSession("session");
    await vi.waitFor(() => expect(reads.plans).toHaveLength(1));
    const newSelection = scope === "session"
      ? client.selectSession("session") : client.selectWorkspace("workspace-b-id");
    if (scope === "workspace") {
      await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
      const socket = FakeWebSocket.instances[1];
      socket.emit("open");
      socket.emit("message", {type: "hello", protocol_version: 1, sequence: 0});
    }
    await vi.waitFor(() => expect(reads.plans).toHaveLength(2));
    expect(reads.plans[0].signal?.aborted).toBe(true);
    expect(reads.agents[0].signal?.aborted).toBe(true);
    expect(reads.plans[1].workspaceID).toBe(scope === "session" ? "workspace-id" : "workspace-b-id");
    expect(reads.agents[1].sessionID).toBe(scope === "session" ? "session" : "session-b");
    reads.plans[1].resolve(envelope({version: 1}));
    reads.agents[1].resolve(envelope({agents: [{agent_id: "current", status: "completed"}]}));
    await newSelection;
    const accepted = client.getSnapshot();
    // The fake transport deliberately completes even after abort.
    reads.plans[0].resolve(envelope({version: 1}));
    reads.agents[0].resolve(envelope({agents: [{agent_id: "old", status: "running"}]}));
    await oldSelection;
    expect(client.getSnapshot()).toBe(accepted);
    client.stop();
  });

  it("cancels progress reads on stop without publishing their late responses", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const reads = deferProgressReads();
    const selecting = client.selectSession("session");
    await vi.waitFor(() => expect(reads.plans).toHaveLength(1));
    client.stop();
    const stopped = client.getSnapshot();
    expect(reads.plans[0].signal?.aborted).toBe(true);
    expect(reads.agents[0].signal?.aborted).toBe(true);
    reads.plans[0].resolve(envelope({version: 1}));
    reads.agents[0].resolve(envelope({agents: [{agent_id: "late", status: "running"}]}));
    await selecting;
    expect(client.getSnapshot()).toBe(stopped);
  });

  it("queries only incomplete and newly loaded Turns and merges their traces", async () => {
    snapshotSequence = 12;
    snapshotTruncatedBefore = 10;
    snapshotEvents = [runtimeEvent(11, "turn.started"), runtimeEvent(12, "turn.completed")];
    traceComplete = false;
    const client = new RuntimeClient();
    const socket = await startClient(client);
    traceComplete = true;
    await client.refreshTrace();
    const count = requests.filter((r) => r.route.endsWith("/trace/query")).length;
    await client.refreshTrace();
    expect(requests.filter((r) => r.route.endsWith("/trace/query"))).toHaveLength(count);
    for (const [sequence, kind] of [[13, "turn.started"], [14, "turn.completed"]] as const) {
      socket.emit("message", {
        type: "event", protocol_version: 1, session_id: "session", sequence,
        event: {...runtimeEvent(sequence, kind), turn_id: "turn-new"}
      });
    }
    await vi.waitFor(() => expect(client.getSnapshot().trace?.turns).toHaveLength(2));
    expect(requests.filter((r) => r.route.endsWith("/trace/query")).at(-1)?.body.turn_ids)
      .toEqual(["turn-new"]);
    earlierEvents = [
      {...runtimeEvent(1, "turn.started"), turn_id: "turn-old"},
      {...runtimeEvent(2, "turn.completed"), turn_id: "turn-old"}
    ];
    await client.loadEarlierHistory();
    await vi.waitFor(() => expect(client.getSnapshot().trace?.turns).toHaveLength(3));
    expect(requests.filter((r) => r.route.endsWith("/trace/query")).at(-1)?.body.turn_ids)
      .toEqual(["turn-old"]);
    client.stop();
  });

  it("restores scoped cursor, selection, and drafts without persisting tokens", async () => {
    const storage = new MemoryBrowserStorage();
    storage.values.set("v1:build:workspace-id", {
      cursor: 41,
      selectedSessionID: "session",
      drafts: {session: "unfinished prompt"},
      messageFeedback: {"session:output-turn": "negative"}
    });
    const client = new RuntimeClient(storage);
    await startClient(client);

    const socket = FakeWebSocket.instances.at(-1);
    socket?.emit("open");
    expect(JSON.parse(socket?.sent[0] ?? "{}")).toEqual({
      type: "authenticate",
      token: "token",
      workspace_id: "workspace-id",
      cursor: 41
    });
    expect(await client.loadDraft()).toBe("unfinished prompt");
    expect(client.getSnapshot().messageFeedback).toEqual({
      "session:output-turn": "negative"
    });

    socket?.emit("message", {
      type: "watermark",
      protocol_version: 1,
      sequence: 42
    });
    client.saveDraft("next prompt");
    client.toggleMessageFeedback("output-turn", "positive");
    await vi.waitFor(() => {
      expect(storage.values.get("v1:build:workspace-id")).toMatchObject({
        cursor: 42,
        selectedSessionID: "session",
        drafts: {session: "next prompt"},
        messageFeedback: {"session:output-turn": "positive"}
      });
    });
    expect(JSON.stringify(storage.values)).not.toContain("token");
    client.stop();
  });

  it("keeps event cursors and selected sessions isolated by Workspace", async () => {
    multipleWorkspaces = true;
    const storage = new MemoryBrowserStorage();
    storage.values.set("v1:build:workspace-id", {
      cursor: 41,
      selectedSessionID: "session",
      drafts: {session: "workspace A"}
    });
    storage.values.set("v1:build:workspace-b-id", {
      cursor: 7,
      selectedSessionID: "session-b",
      drafts: {"session-b": "workspace B"}
    });
    const client = new RuntimeClient(storage);
    const first = await startClient(client);

    expect(JSON.parse(first.sent[0] ?? "{}")).toMatchObject({
      workspace_id: "workspace-id",
      cursor: 41
    });
    expect(await client.loadDraft()).toBe("workspace A");
    const switching = client.selectWorkspace("workspace-b-id");
    await vi.waitFor(() => {
      expect(FakeWebSocket.instances).toHaveLength(2);
    });
    const second = FakeWebSocket.instances[1]!;
    second.emit("open");
    expect(JSON.parse(second.sent[0] ?? "{}")).toMatchObject({
      workspace_id: "workspace-b-id",
      cursor: 7
    });
    second.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 7
    });
    await switching;

    expect(client.getSnapshot()).toMatchObject({
      selectedWorkspaceID: "workspace-b-id",
      workspaceRoot: "/workspace-b",
      selectedSessionID: "session-b"
    });
    expect(await client.loadDraft()).toBe("workspace B");
    const listedWorkspaceIDs = requests
      .filter((request) => request.route.endsWith("/session/list"))
      .map((request) => request.headers.get("X-QCode-Workspace-ID"));
    expect(listedWorkspaceIDs).toContain("workspace-id");
    expect(listedWorkspaceIDs).toContain("workspace-b-id");
    client.stop();
  });

  it.each(["workspace", "session"] as const)(
    "preserves drafts entered before and during a cross-Workspace %s switch",
    async (selection) => {
      multipleWorkspaces = true;
      const storage = new MemoryBrowserStorage();
      const client = new RuntimeClient(storage);
      await startClient(client);
      const firstDraft = "  第一份草稿\n原样保留 🙂  ";
      const lateDraft = `${firstDraft}\n切换期间继续输入`;
      const secondDraft = "第二个 Workspace 的草稿";
      let release!: () => void;
      let loading!: () => void;
      const entered = new Promise<void>((resolve) => { loading = resolve; });
      const gate = new Promise<void>((resolve) => { release = resolve; });
      const load = storage.load.bind(storage);
      storage.load = async (scope) => {
        if (scope === "v1:build:workspace-b-id") {
          loading();
          await gate;
        }
        return load(scope);
      };
      const select = (workspaceID: string, sessionID: string) => selection === "workspace"
        ? client.selectWorkspace(workspaceID)
        : client.selectSession(sessionID);
      const finishSwitch = async (switching: Promise<void>) => {
        const count = FakeWebSocket.instances.length;
        await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(count + 1));
        const socket = FakeWebSocket.instances.at(-1)!;
        socket.emit("open");
        socket.emit("message", {type: "hello", protocol_version: 1, sequence: 0});
        await switching;
      };
      try {
        client.saveDraft(firstDraft);
        const switching = select("workspace-b-id", "session-b");
        await entered;
        expect(client.getSnapshot().selectedWorkspaceID).toBe("workspace-id");
        client.saveDraft(lateDraft);
        expect(client.loadDraft("session")).toBe(lateDraft);
        release();
        await finishSwitch(switching);
        expect(client.loadDraft()).toBe("");
        client.saveDraft(secondDraft);
        await finishSwitch(select("workspace-id", "session"));
        expect(client.loadDraft()).toBe(lateDraft);
        expect(storage.values.get("v1:build:workspace-id")?.drafts)
          .toEqual({session: lateDraft});
        expect(storage.values.get("v1:build:workspace-b-id")?.drafts)
          .toEqual({"session-b": secondDraft});
      } finally {
        release();
        client.stop();
      }
    }
  );

  it.each(["pagehide", "stop"])("flushes the latest draft on %s without waiting for storage debounce", async (exit) => {
    vi.useFakeTimers();
    const storage = new MemoryBrowserStorage();
    const client = new RuntimeClient(storage);
    await startClient(client);
    try {
      const before = storage.saveCalls;
      client.saveDraft("第一行");
      client.saveDraft("第一行\n第二行 🙂 ");
      expect(storage.saveCalls).toBe(before);
      if (exit === "stop") client.stop();
      else window.dispatchEvent(new Event("pagehide"));
      // Drain the serialized storage promise without advancing the debounce.
      await vi.advanceTimersByTimeAsync(0);
      expect(storage.values.get("v1:build:workspace-id")?.drafts)
        .toEqual({session: "第一行\n第二行 🙂 "});
      expect(storage.saveCalls).toBe(before + 1);
    } finally {
      client.stop();
    }
  });

  it("routes background Session lifecycle mutations to their owner Workspace", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);

    await client.updateSession("session-b", 1, {pinned: true});
    await client.deleteSession("session-b", 1);

    const mutations = requests.filter((request) =>
      request.route.endsWith("/session/update") ||
      request.route.endsWith("/session/delete")
    );
    expect(mutations).toHaveLength(2);
    expect(mutations.map((request) =>
      request.headers.get("X-QCode-Workspace-ID")
    )).toEqual(["workspace-b-id", "workspace-b-id"]);
    expect(client.getSnapshot().selectedWorkspaceID).toBe("workspace-id");
    client.stop();
  });

  it("switches a Git branch through the owning Workspace Runtime", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);
    await client.switchWorkspaceBranch("workspace-b-id", "feature");

    const request = requests.find(
      (value) => value.route.endsWith("/workspace/git-switch")
    );
    expect(request?.body).toEqual({branch: "feature"});
    expect(request?.headers.get("X-QCode-Workspace-ID"))
      .toBe("workspace-b-id");
    expect(request?.headers.get("Idempotency-Key")).toBe("request-id");
    client.stop();
  });

  it("binds Git inspection and action requests to a Workspace without consuming the composer context", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);
    await client.gitOverview("workspace-b-id");
    await client.gitPatch("workspace-b-id", "a.txt", true);
    const reads = requests.filter((value) =>
      value.route.endsWith("/workspace/git-status") || value.route.endsWith("/workspace/git-diff"));
    expect(reads.map((value) => value.headers.get("X-QCode-Workspace-ID")))
      .toEqual(["workspace-b-id", "workspace-b-id"]);
    expect(reads[1].body).toEqual({path: "a.txt", staged: true});
    client.saveDraft("unfinished question");
    const action = {action: "commit" as const, branch: "main", revision: "reviewed-index", message: 'fix "quotes"', paths: ["a.txt"], include_unstaged: false};
    await client.executeGitAction("workspace-id", "session", action);
    const submit = requests.find((value) => value.route.endsWith("/workspace/git-action"));
    expect(submit?.headers.get("X-QCode-Workspace-ID")).toBe("workspace-id");
    expect(submit?.headers.get("Idempotency-Key")).toBe("request-id");
    expect(submit?.body).toEqual({session_id: "session", ...action});
    expect(requests.some((value) => value.route.endsWith("/operation/submit"))).toBe(false);
    expect(await client.loadDraft()).toBe("unfinished question");
    await expect(client.executeGitAction("workspace-b-id", "session-b", action)).rejects.toThrow("selected Workspace");
    client.stop();
  });

  it("binds cross-Workspace Session hydration to its owner Runtime", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);
    const before = requests.length;

    const selecting = client.selectSession("session-b");
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    const secondary = FakeWebSocket.instances[1]!;
    secondary.emit("open");
    secondary.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 0
    });
    await selecting;

    const hydrationRoutes = new Set([
      "/session/activate", "/session/snapshot", "/profile/get", "/tool/catalog",
      "/checkpoint/list", "/plan/get", "/agent/list",
      "/usage/query", "/extension/list", "/turn/queue"
    ]);
    const hydration = requests.slice(before).filter((request) =>
      [...hydrationRoutes].some((route) => request.route.endsWith(route)) &&
      request.body.session_id === "session-b"
    );
    expect(hydration.length).toBeGreaterThan(5);
    expect(hydration.every((request) =>
      request.headers.get("X-QCode-Workspace-ID") === "workspace-b-id"
    )).toBe(true);
    expect(client.getSnapshot()).toMatchObject({
      selectedWorkspaceID: "workspace-b-id",
      selectedSessionID: "session-b"
    });
    client.stop();
  });

  it("does not fall back to another Workspace Session when the selected one is empty", async () => {
    multipleWorkspaces = true;
    emptyPrimaryWorkspace = true;
    const client = new RuntimeClient();
    await startClient(client);
    expect(client.getSnapshot()).toMatchObject({
      selectedWorkspaceID: "workspace-id",
      selectedSessionID: ""
    });

    const selectSecondary = client.selectWorkspace("workspace-b-id");
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
    const secondary = FakeWebSocket.instances[1]!;
    secondary.emit("open");
    secondary.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 0
    });
    await selectSecondary;
    expect(client.getSnapshot().selectedSessionID).toBe("session-b");

    const selectPrimary = client.selectWorkspace("workspace-id");
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(3));
    const primary = FakeWebSocket.instances[2]!;
    primary.emit("open");
    primary.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 0
    });
    await selectPrimary;
    expect(client.getSnapshot()).toMatchObject({
      selectedWorkspaceID: "workspace-id",
      selectedSessionID: ""
    });
    client.stop();
  });

  it("requires an explicit ready Workspace before creating a Session", async () => {
    window.history.replaceState(null, "", "/");
    const client = new RuntimeClient();
    await client.start();

    expect(FakeWebSocket.instances).toHaveLength(0);
    expect(client.getSnapshot()).toMatchObject({
      phase: "ready",
      selectedWorkspaceID: "",
      selectedSessionID: ""
    });
    await expect(client.createSession()).rejects.toThrow(
      "Select a ready workspace"
    );
    expect(requests.some(
      (request) => request.route.endsWith("/session/create")
    )).toBe(false);
    client.stop();
  });

  it("submits context compaction against the active Session's latest turn", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.compactThread();

    const submit = requests.filter(
      (request) => request.route.endsWith("/operation/submit")
    ).at(-1);
    expect(submit?.body).toEqual({
      session_id: "session",
      kind: "thread.compact",
      idempotency_key: "request-id",
      payload: {
        thread_id: "thread",
        turn_id: "turn",
        item_id: "compact-request-id"
      }
    });
    client.stop();
  });

  it("discards browser projection state when the cache scope changes", async () => {
    const storage = new MemoryBrowserStorage();
    storage.values.set("v1:old-build:workspace-id", {
      cursor: 99,
      selectedSessionID: "old-session",
      drafts: {"old-session": "stale"}
    });
    const client = new RuntimeClient(storage);
    await startClient(client);

    const socket = FakeWebSocket.instances.at(-1);
    socket?.emit("open");
    expect(JSON.parse(socket?.sent[0] ?? "{}")).toMatchObject({cursor: 0});
    expect(client.getSnapshot().selectedSessionID).toBe("session");
    expect(await client.loadDraft()).toBe("");
    client.stop();
  });

  it("retries session creation with one stable idempotency key", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    failNextCreate = true;

    await client.createSession();

    const creates = requests.filter((request) => request.route.endsWith("/session/create"));
    expect(creates).toHaveLength(2);
    expect(creates[0]?.body).toMatchObject({
      session_id: "session_web_request-id",
      isolation: "shared"
    });
    expect(creates[1]?.body).toMatchObject({
      session_id: "session_web_request-id"
    });
    expect(creates[0]?.headers.get("Idempotency-Key")).toBe("request-id");
    expect(creates[1]?.headers.get("Idempotency-Key")).toBe("request-id");
    client.stop();
  });

  it("inherits approval and applies explicit choices to a new session", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    await client.updateProfile({approval_posture: "auto"});
    requests.length = 0;

    await client.createSession("worktree", {
      model: "reasoner",
      reasoning_effort: "high"
    });

    const update = requests.find(
      (request) => request.route.endsWith("/profile/update")
    );
    expect(update?.body).toMatchObject({
      patch: {
        model: "reasoner",
        reasoning_effort: "high",
        approval_posture: "auto"
      }
    });
    expect(client.getSnapshot().profile?.profile.model).toBe("reasoner");
    client.stop();
  });

  it("sends explicit discard intent when deleting a session", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.deleteSession("session", 1, true);

    expect(requests.find((request) => request.route.endsWith("/session/delete"))?.body)
      .toEqual({
        session_id: "session",
        expected_revision: 1,
        discard: true
      });
    client.stop();
  });

  it("creates, lists, applies, and deletes Runtime-owned Agent presets", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    expect((await client.listAgentPresets()).presets).toEqual([]);

    const saved = await client.saveAgentPreset({
      name: "Review",
      description: "Focused review",
      profile: {
        mode: "plan",
        provider: "fixture",
        model: "fixture",
        enabled_tool_ids: [],
        approval_posture: "suggest",
        execution_target: "local",
        max_steps: 16
      }
    });
    expect(saved.preset).toMatchObject({
      id: "preset-request-id",
      name: "Review",
      revision: 1,
      scope: "workspace"
    });
    const saveRequest = requests.find((request) =>
      request.route.endsWith("/agent-preset/save")
    );
    expect(saveRequest?.headers.get("Idempotency-Key"))
      .toBe("preset-request-id");
    expect((await client.listAgentPresets()).presets).toHaveLength(1);

    const applied = await client.applyAgentPreset("preset-request-id");
    expect(applied.profile_update.profile.mode).toBe("plan");
    expect(requests.find((request) =>
      request.route.endsWith("/agent-preset/apply")
    )?.body).toMatchObject({
      session_id: "session",
      thread_id: "thread",
      preset_id: "preset-request-id",
      expected_profile_revision: 1
    });

    await client.deleteAgentPreset(saved.preset!);
    expect((await client.listAgentPresets()).presets).toEqual([]);
    client.stop();
  });

  it("routes extension inspection and mutation through the control plane", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    const detail = await client.controlExtension(
      "skill",
      "review",
      "detail"
    );
    expect(detail.detail).toEqual({
      name: "review",
      action: "detail"
    });
    await client.setExtensionEnabled("skill", "review", false);

    const controls = requests.filter((request) =>
      request.route.endsWith("/extension/control")
    );
    expect(controls.map((request) => request.body)).toMatchObject([
      {kind: "skill", name: "review", action: "detail"},
      {kind: "skill", name: "review", action: "disable"}
    ]);
    client.stop();
  });

  it("submits server-issued workspace context and clears it after acceptance", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const resource = await client.readWorkspaceResource("src/main.go");
    client.addWorkspaceContext(resource);

    await client.submitPrompt("review this file");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "file",
          source: "composer",
          uri: "file:///workspace/src/main.go",
          path: "src/main.go",
          document_version: 1,
          digest: "a".repeat(64),
          explicit: true
        }]
      }
    });
    expect(client.getSnapshot().contextResources).toEqual([]);
    client.stop();
  });

  it("submits and queues digest-bound attachment context, then clears it", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const textAttachment = {
      kind: "attachment" as const,
      source: "native_picker" as const,
      digest: "d".repeat(64),
      label: "notes.txt",
      media_type: "text/plain",
      content: "review the parser",
      explicit: true as const
    };
    client.addAttachmentContext(textAttachment);

    await client.enqueue("turn-active", "use the note");

    const enqueue = requests.find((request) =>
      request.route.endsWith("/operation/submit") &&
      request.body.kind === "turn.enqueue"
    );
    expect(enqueue?.body).toMatchObject({
      payload: {
        prompt: "use the note",
        context: [textAttachment]
      }
    });
    expect(client.getSnapshot().contextResources).toEqual([]);

    const imageAttachment = {
      kind: "image" as const,
      source: "native_picker" as const,
      digest: "e".repeat(64),
      label: "pasted.png",
      media_type: "image/png",
      content: "iVBORw0KGgo=",
      explicit: true as const
    };
    client.addAttachmentContext(imageAttachment);
    expect(client.getSnapshot().contextResources).toEqual([imageAttachment]);
    client.removeAttachmentContext(imageAttachment.digest);
    expect(client.getSnapshot().contextResources).toEqual([]);
    client.stop();
  });

  it("selects a workspace directory through the authenticated Host route", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await expect(client.pickWorkspaceDirectory()).resolves.toEqual({
      path: "/workspace/selected"
    });
    expect(requests.find(
      (request) => request.route.endsWith("/workspace/select-directory")
    )?.body).toEqual({});
    client.stop();
  });

  it("removes a non-selected Workspace without reconnecting", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);

    await client.removeWorkspace("workspace-b-id");

    expect(requests.find(
      (request) => request.route.endsWith("/workspace/remove")
    )?.body).toEqual({workspace_id: "workspace-b-id"});
    expect(client.getSnapshot().workspaces.map((workspace) => workspace.id))
      .toEqual(["workspace-id"]);
    expect(client.getSnapshot().selectedWorkspaceID).toBe("workspace-id");
    expect(FakeWebSocket.instances).toHaveLength(1);
    client.stop();
  });

  it("switches to another ready Workspace after removing the selected one", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient();
    await startClient(client);

    const removal = client.removeWorkspace("workspace-id");
    await vi.waitFor(() => {
      expect(FakeWebSocket.instances).toHaveLength(2);
    });
    const fallbackSocket = FakeWebSocket.instances[1]!;
    fallbackSocket.emit("open");
    fallbackSocket.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 0
    });
    await removal;

    expect(client.getSnapshot()).toMatchObject({
      selectedWorkspaceID: "workspace-b-id",
      workspaceRoot: "/workspace-b",
      socketConnected: true
    });
    expect(client.getSnapshot().workspaces.map((workspace) => workspace.id))
      .toEqual(["workspace-b-id"]);
    expect(FakeWebSocket.instances).toHaveLength(2);
    client.stop();
  });

  it("enters the empty Workspace state after removing the last one", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.removeWorkspace("workspace-id");

    expect(client.getSnapshot()).toMatchObject({
      phase: "ready",
      workspaceRoot: "",
      workspaces: [],
      selectedWorkspaceID: "",
      sessions: [],
      selectedSessionID: "",
      socketConnected: false
    });
    expect(FakeWebSocket.instances).toHaveLength(1);
    client.stop();
  });

  it("submits a server-issued selection range without trusting a browser path", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const resource = await client.readWorkspaceResource("src/main.go");
    client.addWorkspaceContext(resource, {
      start: {line: 0, character: 0},
      end: {line: 0, character: 7}
    });

    await client.submitPrompt("review this selection");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "selection",
          source: "composer",
          uri: "file:///workspace/src/main.go",
          path: "src/main.go",
          document_version: 1,
          digest: "a".repeat(64),
          range: {
            start: {line: 0, character: 0},
            end: {line: 0, character: 7}
          },
          explicit: true
        }]
      }
    });
    client.stop();
  });

  it("submits only server-issued image metadata as native context", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const image = await client.readWorkspaceImage("diagram.png");
    client.addImageContext(image);

    await client.submitPrompt("inspect this image");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "image",
          source: "native_picker",
          uri: "file:///workspace/diagram.png",
          path: "diagram.png",
          document_version: 1,
          digest: "c".repeat(64),
          label: "diagram.png",
          media_type: "image/png",
          explicit: true
        }]
      }
    });
    client.stop();
  });

  it("submits only a server-issued repository symbol range", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const result = await client.searchWorkspaceSymbols("main");
    client.addSymbolContext(result.symbols[0]!);

    await client.submitPrompt("inspect this symbol");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "symbol",
          source: "native_picker",
          path: "src/main.go",
          digest: "a".repeat(64),
          symbol: {
            name: "main",
            kind: "function"
          }
        }]
      }
    });
    client.stop();
  });

  it("submits only a persisted diagnostic receipt context", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const result = await client.workspaceDiagnostics();
    client.addDiagnosticsContext(result.diagnostics[0]!);

    await client.submitPrompt("fix this diagnostic");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "diagnostics",
          source: "code_action",
          path: "src/main.go",
          diagnostics: [{
            severity: "error",
            message: "broken"
          }]
        }]
      }
    });
    client.stop();
  });

  it("downloads a signed content handle with the in-memory capability token", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    const content = await client.downloadWorkspaceContent("signed.handle");

    expect(content.size).toBe(13);
    expect(content.type).toBe("text/plain;charset=utf-8");
    const request = requests.find((item) => item.route.includes("/api/v1/content/"));
    expect(request?.route).toContain("signed.handle");
    expect(request?.headers.get("Authorization")).toBe("Bearer token");
    client.stop();
  });

  it("submits only the server-issued Git diff as inline context", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const diff = await client.workspaceDiff();
    client.addGitDiffContext(diff);

    await client.submitPrompt("review this diff");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "git_diff",
          source: "composer",
          digest: "b".repeat(64),
          label: "Current workspace diff",
          media_type: "text/plain",
          content: "diff --git a/main.go b/main.go\n",
          explicit: true
        }]
      }
    });
    client.stop();
  });

  it("binds terminal context to the projected tool call and output digest", async () => {
    const client = new RuntimeClient();
    await startClient(client);

    await client.addTerminalContext("call-1", "terminal output");
    await client.submitPrompt("inspect this output");

    const submit = requests.find((request) => request.route.endsWith("/operation/submit"));
    expect(submit?.body).toMatchObject({
      payload: {
        context: [{
          kind: "terminal",
          source: "composer",
          digest: "ab".repeat(32),
          label: "call-1",
          media_type: "text/plain",
          content: "terminal output",
          explicit: true
        }]
      }
    });
    client.stop();
  });

  it("prepends an older history page without duplicating snapshot events", async () => {
    snapshotSequence = 12;
    snapshotTruncatedBefore = 9;
    snapshotEvents = [
      runtimeEvent(10, "output.delta"),
      runtimeEvent(11, "turn.receipt"),
      runtimeEvent(12, "turn.completed")
    ];
    earlierEvents = [
      runtimeEvent(8, "output.delta"),
      runtimeEvent(9, "turn.completed"),
      runtimeEvent(10, "output.delta")
    ];
    moreBefore = true;
    const client = new RuntimeClient();
    await startClient(client);

    expect(client.getSnapshot().historyMoreBefore).toBe(true);
    expect(await client.loadEarlierHistory()).toBe(2);
    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([
      8, 9, 10, 11, 12
    ]);
    expect(client.getSnapshot().historyMoreBefore).toBe(true);
    expect(requests.find((request) => request.route.endsWith("/session/history"))?.body)
      .toMatchObject({
        session_id: "session",
        before_sequence: 10,
        limit: 200
      });
    client.stop();
  });

  it("coalesces history requests and preserves live events received while loading", async () => {
    snapshotSequence = 12;
    snapshotTruncatedBefore = 9;
    snapshotEvents = [runtimeEvent(10, "output.delta"), runtimeEvent(12, "turn.completed")];
    earlierEvents = [runtimeEvent(8, "output.delta"), runtimeEvent(9, "turn.completed")];
    let release!: () => void;
    historyGate = new Promise<void>((resolve) => { release = resolve; });
    const client = new RuntimeClient();
    const socket = await startClient(client);
    const first = client.loadEarlierHistory();
    expect(client.loadEarlierHistory()).toBe(first);
    expect(requests.filter((request) => request.route.endsWith("/session/history"))).toHaveLength(1);
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "session", sequence: 13,
      event: runtimeEvent(13, "output.delta")
    });
    release();
    expect(await first).toBe(2);
    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([8, 9, 10, 12, 13]);
    expect(client.getSnapshot().historyMoreBefore).toBe(false);
    client.stop();
  });

  it("cancels and discards history responses even after returning to the same Session", async () => {
    snapshotSequence = 12;
    snapshotTruncatedBefore = 9;
    snapshotEvents = [runtimeEvent(10, "output.delta"), runtimeEvent(12, "turn.completed")];
    earlierEvents = [runtimeEvent(8, "turn.completed")];
    let release!: () => void;
    historyGate = new Promise<void>((resolve) => { release = resolve; });
    const client = new RuntimeClient();
    await startClient(client);
    const pending = client.loadEarlierHistory();
    const historyCall = vi.mocked(fetch).mock.calls.find(([url]) => String(url).endsWith("/session/history"));
    await client.selectSession("session-b");
    expect(historyCall?.[1]?.signal?.aborted).toBe(true);
    snapshotEvents = [runtimeEvent(20, "turn.completed")];
    snapshotSequence = 20;
    await client.selectSession("session");
    release();
    expect(await pending).toBe(0);
    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([20]);
    client.stop();
  });

  it("rejects a non-advancing history cursor and allows an explicit retry", async () => {
    snapshotSequence = 10;
    snapshotTruncatedBefore = 9;
    snapshotEvents = [runtimeEvent(10, "turn.completed")];
    earlierEvents = [runtimeEvent(10, "turn.completed")];
    moreBefore = true;
    const client = new RuntimeClient();
    await startClient(client);
    await expect(client.loadEarlierHistory()).rejects.toThrow("History did not advance");
    earlierEvents = [runtimeEvent(9, "turn.completed")];
    moreBefore = false;
    expect(await client.loadEarlierHistory()).toBe(1);
    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([9, 10]);
    client.stop();
  });

  it("merges live events received while a session snapshot hydrates", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const socket = FakeWebSocket.instances.at(-1);
    if (!socket) throw new Error("missing WebSocket");
    snapshotSequence = 5;
    let releaseProfile = (): void => {};
    profileGate = new Promise<void>((resolve) => {
      releaseProfile = resolve;
    });

    const selecting = client.selectSession("session");
    await vi.waitFor(() => {
      expect(
        requests.filter((request) => request.route.endsWith("/profile/get"))
      ).toHaveLength(2);
    });
    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 6,
      event: runtimeEvent(6, "output.delta")
    });
    releaseProfile();
    await selecting;

    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([6]);
    client.stop();
  });

  it("coalesces session-list invalidation for foreign lifecycle events", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const socket = FakeWebSocket.instances.at(-1);
    if (!socket) throw new Error("missing WebSocket");
    const before = requests.filter(
      (request) => request.route.endsWith("/session/list")
    ).length;
    for (let sequence = 20; sequence < 70; sequence += 1) {
      socket.emit("message", {
        type: "event",
        protocol_version: 1,
        session_id: "foreign-session",
        sequence,
        event: runtimeEvent(sequence, "output.delta")
      });
    }
    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "foreign-session",
      sequence: 70,
      event: runtimeEvent(70, "turn.started")
    });
    for (let sequence = 71; sequence < 75; sequence += 1) {
      socket.emit("message", {
        type: "event",
        protocol_version: 1,
        session_id: "session",
        sequence,
        event: runtimeEvent(sequence, "approval.resolved")
      });
    }
    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "foreign-session",
      sequence: 75,
      event: runtimeEvent(75, "turn.completed")
    });
    await vi.waitFor(() => {
      expect(
        requests.filter((request) => request.route.endsWith("/session/list"))
          .length
      ).toBe(before + 1);
    });
    const after = requests.filter(
      (request) => request.route.endsWith("/session/list")
    ).length;
    expect(after - before).toBe(1);
    client.stop();
  });

  it("coalesces in-flight activity across event tasks and preserves other Workspace search results", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient(new MemoryBrowserStorage());
    const socket = await startClient(client);
    await client.refreshSessions("needle");
    const before = client.getSnapshot();
    const primary = before.sessions.find((session) => session.session_id === "session")!;
    const secondary = before.sessions.find((session) => session.session_id === "session-b")!;
    const {reads} = deferSessionReads();
    const emit = (sequence: number) => socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "unknown-session", sequence,
      event: runtimeEvent(sequence, "session.title.updated")
    });
    emit(20);
    await vi.waitFor(() => expect(reads).toHaveLength(2));
    for (let sequence = 21; sequence < 26; sequence++) {
      emit(sequence);
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    expect(reads).toHaveLength(2);
    expect(reads.every((read) => !read.signal?.aborted)).toBe(true);
    for (const read of reads.slice()) {
      read.resolve(envelope({version: 1, sessions: [{...primary, title: "First"}]}));
    }
    await vi.waitFor(() => expect(reads).toHaveLength(4));
    expect(reads.map((read) => read.workspaceID))
      .toEqual(Array(4).fill("workspace-id"));
    expect(reads.map((read) => read.query)).toEqual(["", "needle", "", "needle"]);
    for (const read of reads.slice(2)) {
      read.resolve(envelope({version: 1, sessions: [{...primary, title: "Latest"}]}));
    }
    await vi.waitFor(() => expect(client.getSnapshot().sessions
      .find((session) => session.session_id === "session")?.title).toBe("Latest"));
    expect(reads).toHaveLength(4);
    expect(client.getSnapshot().sessions.find((session) => session.session_id === "session-b"))
      .toBe(secondary);
    expect(client.getSnapshot().sessionSearchResults.find((session) => session.session_id === "session-b"))
      .toBe(secondary);
    expect(client.getSnapshot().sessionSearchQuery).toBe("needle");
    expect(client.getSnapshot().selectedSessionID).toBe(before.selectedSessionID);
    expect(client.getSnapshot().profile).toBe(before.profile);
    client.stop();
  });

  it("waits for an in-flight search before applying scoped activity invalidation", async () => {
    multipleWorkspaces = true;
    const client = new RuntimeClient(new MemoryBrowserStorage());
    const socket = await startClient(client);
    const sessions = client.getSnapshot().sessions;
    const {reads} = deferSessionReads();
    const searching = client.refreshSessions("needle");
    expect(reads).toHaveLength(2);
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "unknown-session", sequence: 20,
      event: runtimeEvent(20, "session.title.updated")
    });
    await Promise.resolve();
    expect(reads).toHaveLength(2);
    expect(reads.every((read) => !read.signal?.aborted)).toBe(true);
    for (const read of reads.slice()) {
      read.resolve(envelope({version: 1, sessions: sessions.filter((session) =>
        session.workspace_root === (read.workspaceID === "workspace-id" ? "/workspace" : "/workspace-b"))}));
    }
    await searching;
    await vi.waitFor(() => expect(reads).toHaveLength(4));
    expect(reads.slice(2).map((read) => read.workspaceID)).toEqual(["workspace-id", "workspace-id"]);
    const replacement = client.refreshSessions("latest");
    expect(reads.slice(2, 4).every((read) => read.signal?.aborted)).toBe(true);
    // The old transport deliberately ignores cancellation.
    for (const read of reads.slice(2, 4)) read.resolve(envelope({version: 1, sessions: []}));
    for (const read of reads.slice(4)) read.resolve(envelope({version: 1, sessions: []}));
    await replacement;
    await vi.waitFor(() => expect(reads).toHaveLength(8));
    expect(reads.slice(6).map((read) => read.query)).toEqual(["", "latest"]);
    for (const read of reads.slice(6)) read.resolve(envelope({version: 1, sessions: []}));
    await vi.waitFor(() => expect(client.getSnapshot().sessionSearchResults).toEqual([]));
    expect(client.getSnapshot().sessionSearchQuery).toBe("latest");
    client.stop();
  });

  it.each(["stop", "switch"])("discards queued activity and late list responses on %s", async (action) => {
    multipleWorkspaces = true;
    const client = new RuntimeClient(new MemoryBrowserStorage());
    const socket = await startClient(client);
    const {reads, originalFetch} = deferSessionReads();
    for (const sequence of [20, 21]) {
      socket.emit("message", {
        type: "event", protocol_version: 1, session_id: "unknown-session", sequence,
        event: runtimeEvent(sequence, "session.title.updated")
      });
      await Promise.resolve();
    }
    expect(reads).toHaveLength(1);
    vi.stubGlobal("fetch", originalFetch);
    if (action === "stop") {
      client.stop();
    } else {
      const switching = client.selectWorkspace("workspace-b-id");
      await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
      const nextSocket = FakeWebSocket.instances[1]!;
      nextSocket.emit("open");
      nextSocket.emit("message", {type: "hello", protocol_version: 1, sequence: 0});
      await switching;
      expect(client.getSnapshot().selectedWorkspaceID).toBe("workspace-b-id");
    }
    expect(reads[0]!.signal?.aborted).toBe(true);
    const before = client.getSnapshot();
    const count = requests.filter((request) => request.route.endsWith("/session/list")).length;
    reads[0]!.resolve(envelope({version: 1, sessions: []}));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(client.getSnapshot()).toBe(before);
    expect(requests.filter((request) => request.route.endsWith("/session/list"))).toHaveLength(count);
    client.stop();
  });

  it.each(["session", "foreign-session"])("refreshes automatic titles for %s without a chat message", async (sessionID) => {
    const client = new RuntimeClient();
    const socket = await startClient(client);
    const before = requests.filter((request) => request.route.endsWith("/session/list")).length;
    const order = client.getSnapshot().conversation.order;
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: sessionID, sequence: 20,
      event: {...runtimeEvent(20, "session.title.updated"), data: {
        session_id: sessionID, title: "Inspect parser behavior", title_source: "auto", title_revision: 3
      }}
    });
    await vi.waitFor(() => expect(
      requests.filter((request) => request.route.endsWith("/session/list"))
    ).toHaveLength(before + 1));
    expect(client.getSnapshot().conversation.order).toEqual(order);
    client.stop();
  });

  it("refreshes authoritative Session activity for a foreign approval", async () => {
    const client = new RuntimeClient();
    const socket = await startClient(client);
    const before = requests.filter(
      (request) => request.route.endsWith("/session/list")
    ).length;

    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "foreign-session",
      sequence: 20,
      event: runtimeEvent(20, "approval.required")
    });

    await vi.waitFor(() => {
      expect(
        requests.filter((request) => request.route.endsWith("/session/list"))
          .length
      ).toBe(before + 1);
    });
    client.stop();
  });

  it("buffers live events before the session snapshot returns", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    const socket = FakeWebSocket.instances.at(-1);
    if (!socket) throw new Error("missing WebSocket");
    snapshotSequence = 5;
    let releaseSnapshot = (): void => {};
    snapshotGate = new Promise<void>((resolve) => {
      releaseSnapshot = resolve;
    });

    const selecting = client.selectSession("session");
    await vi.waitFor(() => {
      expect(
        requests.filter((request) => request.route.endsWith("/session/snapshot"))
      ).toHaveLength(2);
    });
    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 6,
      event: runtimeEvent(6, "output.delta")
    });
    releaseSnapshot();
    await selecting;

    expect(client.getSnapshot().events.map((event) => event.sequence)).toEqual([6]);
    client.stop();
  });

  it("changes operation ownership immediately while a Session hydrates", async () => {
    const client = new RuntimeClient();
    await startClient(client);
    let releaseSnapshot = (): void => {};
    snapshotGate = new Promise<void>((resolve) => {
      releaseSnapshot = resolve;
    });

    const selecting = client.selectSession("session-b");
    expect(client.getSnapshot()).toMatchObject({
      selectedSessionID: "session-b",
      hydratingSessionID: "session-b",
      events: [],
      profile: undefined
    });
    await expect(client.submitPrompt("must not reach session-a")).rejects.toThrow(
      "Session is still loading"
    );

    releaseSnapshot();
    await selecting;
    expect(client.getSnapshot().hydratingSessionID).toBe("");
    client.stop();
  });

  it("advances the replay cursor from payload-free watermark frames", async () => {
    vi.useFakeTimers();
    const client = new RuntimeClient();
    await startClient(client);
    const first = FakeWebSocket.instances.at(-1);
    if (!first) throw new Error("missing WebSocket");
    first.emit("message", {
      type: "watermark",
      protocol_version: 1,
      sequence: 19
    });
    first.emit("close");
    await vi.advanceTimersByTimeAsync(700);
    const second = FakeWebSocket.instances[1];
    second?.emit("open");

    expect(JSON.parse(second?.sent[0] ?? "{}")).toMatchObject({cursor: 19});
    client.stop();
  });

  it("publishes streaming deltas at most once per animation frame", async () => {
    let flush: FrameRequestCallback | undefined;
    vi.stubGlobal("requestAnimationFrame", vi.fn((callback: FrameRequestCallback) => {
      flush = callback;
      return 1;
    }));
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    const client = new RuntimeClient();
    const socket = await startClient(client);
    let notifications = 0;
    const unsubscribe = client.subscribe(() => {
      notifications += 1;
    });

    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 1,
      event: runtimeEvent(1, "output.delta")
    });
    socket.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 2,
      event: runtimeEvent(2, "output.delta")
    });

    expect(notifications).toBe(0);
    flush?.(performance.now());
    expect(notifications).toBe(1);
    expect(client.getSnapshot().events).toHaveLength(2);
    unsubscribe();
    client.stop();
  });

  it("writes a burst of cursor updates as one browser-state checkpoint", async () => {
    const storage = new MemoryBrowserStorage();
    const client = new RuntimeClient(storage);
    const socket = await startClient(client);
    await new Promise((resolve) => setTimeout(resolve, 120));
    storage.saveCalls = 0;

    for (let sequence = 1; sequence <= 100; sequence += 1) {
      socket.emit("message", {
        type: "watermark",
        protocol_version: 1,
        sequence
      });
    }
    await new Promise((resolve) => setTimeout(resolve, 120));

    expect(storage.saveCalls).toBe(1);
    expect(storage.values.get("v1:build:workspace-id")?.cursor).toBe(100);
    client.stop();
  });

  it("bootstraps a fresh token before reconnecting the socket", async () => {
    vi.useFakeTimers();
    const client = new RuntimeClient();
    await startClient(client);
    const first = FakeWebSocket.instances.at(-1);
    if (!first) throw new Error("missing first WebSocket");
    bootstrapToken = "rotated-token";

    first.emit("close");
    await vi.advanceTimersByTimeAsync(700);
    await vi.waitFor(() => {
      expect(FakeWebSocket.instances).toHaveLength(2);
    });
    const second = FakeWebSocket.instances[1];
    second?.emit("open");

    expect(JSON.parse(second?.sent[0] ?? "{}")).toMatchObject({
      type: "authenticate",
      token: "rotated-token"
    });
    client.stop();
  });

  it("disconnects live progress when the event stream closes", async () => {
    vi.useFakeTimers();
    snapshotEvents = [runtimeEvent(1, "turn.started")];
    snapshotSequence = 1;
    const client = new RuntimeClient();
    const socket = await startClient(client);

    expect(client.getSnapshot().conversation.activeTurnID).toBe("turn");
    socket.emit("close");

    expect(client.getSnapshot()).toMatchObject({
      phase: "failed",
      socketConnected: false,
      problem: {message: "Connection interrupted.", retryable: true}
    });
    client.stop();
  });

  it("authenticates the event stream before requesting hydration data", async () => {
    const client = new RuntimeClient();
    const starting = client.start();
    await vi.waitFor(() => {
      expect(FakeWebSocket.instances).toHaveLength(1);
    });
    expect(requests).toEqual([]);

    const socket = FakeWebSocket.instances[0]!;
    socket.emit("open");
    socket.emit("message", {
      type: "hello",
      protocol_version: 1,
      sequence: 0
    });
    await starting;

    expect(requests.some((request) => request.route.endsWith("/session/snapshot")))
      .toBe(true);
    client.stop();
  });

  it("preserves requested hydration when a background list supersedes startup", async () => {
    snapshotSequence = 3;
    snapshotEvents = [
      {...runtimeEvent(1, "turn.started"), data: {prompt: "Inspect"}},
      {...runtimeEvent(2, "commentary.completed"), data: {
        message_id: "message", sample_id: "sample", text: "Checking callers.", call_ids: ["read"]
      }},
      {...runtimeEvent(3, "turn.completed"), data: {text: "Done"}}
    ];
    const originalFetch = globalThis.fetch;
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let firstList = false;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/session/list") && !firstList) {
        firstList = true;
        await gate;
      }
      return originalFetch(input, init);
    }));
    const client = new RuntimeClient();
    const starting = client.start();
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    const socket = FakeWebSocket.instances[0]!;
    socket.emit("open");
    socket.emit("message", {type: "hello", protocol_version: 1, sequence: 0});
    await vi.waitFor(() => expect(firstList).toBe(true));
    await client.refreshSessions("", false);
    release!();
    await starting;
    expect(client.getSnapshot().events.map((event) => event.kind))
      .toEqual(["turn.started", "commentary.completed", "turn.completed"]);
    expect(requests.filter((request) => request.route.endsWith("/session/snapshot")))
      .toHaveLength(1);
    client.stop();
  });

  it("keeps search results and the active session through activity refresh and empty results", async () => {
    createdSession = true;
    const client = new RuntimeClient(new MemoryBrowserStorage());
    const socket = await startClient(client);
    const originalFetch = globalThis.fetch;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const result = await originalFetch(input, init);
      if (!String(input).endsWith("/session/list")) return result;
      const body = JSON.parse(String(init?.body ?? "{}"));
      if (!body.query) return result;
      return envelope({
        version: 1, query: body.query,
        sessions: body.query === "needle"
          ? client.getSnapshot().sessions.filter((session) => session.session_id === "session-new")
          : []
      });
    }));
    const before = client.getSnapshot();
    const activations = requests.filter((request) => request.route.endsWith("/session/activate")).length;
    await client.refreshSessions("needle");
    expect(client.getSnapshot().sessionSearchResults.map((session) => session.session_id))
      .toEqual(["session-new"]);
    expect(client.getSnapshot().sessions).toHaveLength(2);
    expect(client.getSnapshot().selectedSessionID).toBe(before.selectedSessionID);
    expect(client.getSnapshot().profile).toBe(before.profile);
    requests.length = 0;
    socket.emit("message", {
      type: "event", protocol_version: 1, session_id: "session", sequence: 1,
      event: runtimeEvent(1, "turn.completed")
    });
    await vi.waitFor(() => expect(requests.some((request) =>
      request.route.endsWith("/session/list") && request.body.query === "needle")).toBe(true));
    expect(client.getSnapshot().sessionSearchQuery).toBe("needle");
    expect(client.getSnapshot().sessionSearchResults.map((session) => session.session_id))
      .toEqual(["session-new"]);
    await client.refreshSessions("no match");
    expect(client.getSnapshot().sessionSearchResults).toEqual([]);
    expect(client.getSnapshot().selectedSessionID).toBe(before.selectedSessionID);
    await client.refreshSessions("");
    expect(client.getSnapshot().sessionSearchQuery).toBe("");
    expect(client.getSnapshot().sessions).toHaveLength(2);
    expect(client.getSnapshot().selectedSessionID).toBe(before.selectedSessionID);
    // No filter change should submit a session activation.
    expect(activations).toBeGreaterThan(0);
    expect(requests.filter((request) => request.route.endsWith("/session/activate"))).toEqual([]);
    client.stop();
  });

  it("aborts superseded search requests and ignores responses after replacement or stop", async () => {
    const client = new RuntimeClient(new MemoryBrowserStorage());
    await startClient(client);
    const originalFetch = globalThis.fetch;
    const pending = new Map<string, {
      signal: AbortSignal; release: () => void;
    }>();
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const query = JSON.parse(String(init?.body ?? "{}")).query;
      if (String(input).endsWith("/session/list") && query) {
        // Ignore abort in this fake transport to exercise the late-response guard.
        await new Promise<void>((resolve) => pending.set(query, {
          signal: init!.signal!, release: resolve
        }));
      }
      return originalFetch(input, init);
    }));
    const old = client.refreshSessions("old");
    const latest = client.refreshSessions("latest");
    expect(pending.get("old")!.signal.aborted).toBe(true);
    pending.get("latest")!.release();
    await latest;
    const accepted = client.getSnapshot();
    pending.get("old")!.release();
    await old;
    expect(client.getSnapshot()).toBe(accepted);
    const stopped = client.refreshSessions("stopped");
    client.stop();
    const stoppedSnapshot = client.getSnapshot();
    expect(pending.get("stopped")!.signal.aborted).toBe(true);
    pending.get("stopped")!.release();
    await stopped;
    expect(client.getSnapshot()).toBe(stoppedSnapshot);
  });

  it("preserves search when toggling archived sessions", async () => {
    const client = new RuntimeClient(new MemoryBrowserStorage());
    await startClient(client);
    await client.refreshSessions("中文");
    requests.length = 0;
    await client.setArchivedVisible(true);
    expect(client.getSnapshot().sessionSearchQuery).toBe("中文");
    expect(requests.filter((request) => request.route.endsWith("/session/list"))
      .map((request) => request.body)).toEqual([
      {query: "", include_archived: true, limit: 200},
      {query: "中文", include_archived: true, limit: 200}
    ]);
    client.stop();
  });

  it("clears an expired cursor before reconnecting after desync", async () => {
    const storage = new MemoryBrowserStorage();
    storage.values.set("v1:build:workspace-id", {
      cursor: 41,
      selectedSessionID: "session",
      drafts: {}
    });
    const client = new RuntimeClient(storage);
    const first = await startClient(client);

    first.emit("message", {
      type: "desync",
      protocol_version: 1,
      sequence: 41,
      problem: {
        version: 1,
        code: "conflict",
        message: "cursor history expired",
        retryable: false
      }
    });
    expect(client.getSnapshot().phase).toBe("desynchronized");

    const second = await startClient(client);
    expect(JSON.parse(second.sent[0] ?? "{}")).toMatchObject({cursor: 0});
    expect(client.getSnapshot().phase).toBe("ready");
    client.stop();
  });

  it("fails closed on unknown protocol versions and event kinds", async () => {
    const client = new RuntimeClient();
    const socket = await startClient(client);

    socket.emit("message", {
      type: "event",
      protocol_version: 2,
      session_id: "session",
      sequence: 9,
      event: runtimeEvent(9, "future.event")
    });

    expect(client.getSnapshot().phase).toBe("desynchronized");
    const next = await startClient(client);
    expect(JSON.parse(next.sent[0] ?? "{}")).toMatchObject({cursor: 0});
    next.emit("message", {
      type: "event",
      protocol_version: 1,
      session_id: "session",
      sequence: 10,
      event: runtimeEvent(10, "future.event")
    });
    expect(client.getSnapshot().phase).toBe("desynchronized");
    client.stop();
  });
});

function runtimeEvent(
  sequence: number,
  kind: string,
  data: Record<string, unknown> = {}
) {
  return {
    version: 1,
    id: `event-${sequence}`,
    kind,
    operation_id: "operation",
    thread_id: "thread",
    turn_id: "turn",
    item_id: "item",
    sequence,
    created_at: "2026-01-01T00:00:00Z",
    data
  };
}

function modelCapabilities(displayName: string) {
  return {
    display_name: displayName,
    context_window: 128_000,
    max_output_tokens: 8_192,
    streaming: true,
    reasoning: false,
    tool_calls: true,
    parallel_tool_calls: "unknown",
    native_search: false,
    vision: false,
    image_input: false,
    prompt_cache: true,
    credential_status: "configured",
    availability: "available",
    selection_mode: "hot"
  };
}

function envelope(result: unknown): Response {
  return response({version: 1, result});
}

function envelopeProblem(code: string, message: string, retryable: boolean): Response {
  return new Response(JSON.stringify({
    version: 1,
    problem: {version: 1, code, message, retryable}
  }), {
    status: 503,
    headers: {"Content-Type": "application/json"}
  });
}

class MemoryBrowserStorage implements BrowserStorage {
  readonly values = new Map<string, BrowserProjectionState>();
  saveCalls = 0;

  async load(scope: string): Promise<BrowserProjectionState | undefined> {
    return this.values.get(scope);
  }

  async save(scope: string, state: BrowserProjectionState): Promise<void> {
    this.saveCalls += 1;
    this.values.set(scope, structuredClone(state));
  }
}

function response(value: unknown): Response {
  return new Response(JSON.stringify(value), {
    status: 200,
    headers: {"Content-Type": "application/json"}
  });
}
