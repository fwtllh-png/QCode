import type {RuntimeEvent} from "../protocol";

export type ToolVariant =
  | "read"
  | "search"
  | "shell"
  | "write"
  | "diff"
  | "agent"
  | "generic";

export interface ProjectedEditPlanFile {
  readonly path: string;
  readonly kind: string;
  readonly before: string;
  readonly after: string;
  readonly beforeExists: boolean;
  readonly afterExists: boolean;
}

export interface ProjectedEditPlan {
  readonly id: string;
  readonly diff: string;
  readonly files: readonly ProjectedEditPlanFile[];
}

export interface ProjectedDeliverable {
  readonly path: string;
  readonly tool: string;
  readonly kind: string;
  readonly added: number;
  readonly removed: number;
  readonly summary: string;
  readonly callID?: string;
  readonly diff?: ProjectedEditPlanFile;
  readonly stale: boolean;
}

export interface ProjectedUserImage {
  readonly label: string;
  readonly mediaType: string;
  readonly content: string;
}

export interface ProjectedAgentActivity {
  readonly id: string;
  readonly sequence: number;
  readonly kind: "status" | "reasoning" | "tool" | "message";
  readonly title: string;
  readonly summary: string;
  readonly state: "running" | "completed" | "failed";
  readonly callID?: string;
}

export type ConversationNode =
  | {
      readonly id: string;
      readonly kind: "user";
      readonly turnID: string;
      readonly sequence: number;
      readonly text: string;
      readonly images: readonly ProjectedUserImage[];
      readonly steering?: boolean;
      readonly withdrawn?: boolean;
    }
  | {
      readonly id: string;
      readonly kind: "assistant";
      readonly turnID: string;
      readonly sequence: number;
      readonly text: string;
      readonly superseded?: boolean;
    }
  | {
      readonly id: string;
      readonly kind: "commentary";
      readonly turnID: string;
      readonly sequence: number;
      readonly text: string;
      readonly sampleID: string;
      readonly callIDs: readonly string[];
    }
  | {
      readonly id: string;
      readonly kind: "reasoning";
      readonly turnID: string;
      readonly sequence: number;
      readonly text: string;
      readonly summary: string;
      readonly running: boolean;
    }
  | {
      readonly id: string;
      readonly kind: "tool";
      readonly turnID: string;
      readonly sequence: number;
      readonly callID: string;
      readonly tool: string;
      readonly variant: ToolVariant;
      readonly title: string;
      readonly summary: string;
      readonly state: "running" | "completed" | "failed";
      readonly arguments: unknown;
      readonly output: string;
      readonly errorSummary: string;
      readonly execution?: Record<string, unknown>;
      readonly command?: {
        readonly command: string;
        readonly status: string;
        readonly exitCode?: number;
        readonly durationMS?: number;
      };
      readonly truncated: boolean;
      readonly changes: readonly Record<string, unknown>[];
      readonly editPlan?: ProjectedEditPlan;
      readonly approvalDecision?: string;
      readonly recovery?: Record<string, unknown>;
      readonly contextText?: string;
    }
  | {
      readonly id: string;
      readonly kind: "status";
      readonly turnID: string;
      readonly sequence: number;
      readonly title: string;
      readonly text: string;
      readonly failed: boolean;
      readonly blocked?: boolean;
      readonly warning?: boolean;
      readonly recoverable: boolean;
      readonly recovery?: {
        readonly canRetry: boolean;
        readonly canContinue: boolean;
        readonly sideEffects: string;
        readonly action: string;
      };
    }
  | {
      readonly id: string;
      readonly kind: "receipt";
      readonly turnID: string;
      readonly sequence: number;
      readonly data: Readonly<Record<string, unknown>>;
    }
  | {
      readonly id: string;
      readonly kind: "deliverables";
      readonly turnID: string;
      readonly sequence: number;
      readonly files: readonly ProjectedDeliverable[];
      readonly verification: "passed" | "failed" | "unverified";
    }
  | {
      readonly id: string;
      readonly kind: "context";
      readonly turnID: string;
      readonly sequence: number;
      readonly title: string;
      readonly summary: string;
      readonly data: Readonly<Record<string, unknown>>;
    }
  | {
      readonly id: string;
      readonly kind: "agent";
      readonly turnID: string;
      readonly sequence: number;
      readonly agentID: string;
      readonly role: string;
      readonly taskName: string;
      readonly status: string;
      readonly summary: string;
      readonly state: "running" | "completed" | "failed";
      readonly reasonCode?: string;
      readonly usage?: {readonly inputTokens: number; readonly outputTokens: number};
      readonly activities: readonly ProjectedAgentActivity[];
    };

export interface ConversationSnapshot {
  readonly order: readonly string[];
  readonly nodes: ReadonlyMap<string, ConversationNode>;
  readonly activeTurnID: string;
  readonly activeStatus?: string;
  readonly pendingApproval?: RuntimeEvent;
  readonly pendingInput?: RuntimeEvent;
  readonly revision: number;
}

const emptyConversation: ConversationSnapshot = Object.freeze({
  order: Object.freeze([]),
  nodes: new Map(),
  activeTurnID: "",
  revision: 0
});

export function emptyConversationSnapshot(): ConversationSnapshot {
  return emptyConversation;
}

export function projectConversation(
  events: readonly RuntimeEvent[]
): ConversationSnapshot {
  const projection = new ConversationProjection();
  projection.applyAll(events);
  return projection.snapshot();
}

export class ConversationProjection {
  private readonly order: string[] = [];
  private readonly nodes = new Map<string, ConversationNode>();
  private readonly outputByTurn = new Map<string, string>();
  private readonly reasoningByTurn = new Map<string, string>();
  // One open segment per output node per source: draft deltas are sample
  // scoped and retracted by "output.discarded", while replayed "output.delta"
  // events carry no sample and always append.
  private readonly outputSegments = new Map<string, {sampleID: string; text: string}[]>();
  private readonly ids = new Map<string, string>();
  private readonly activeTurns = new Set<string>();
  private readonly activities = new Map<string, string>();
  private readonly runningTools = new Map<string, Set<string>>();
  private readonly approvals = new Map<string, RuntimeEvent>();
  private readonly inputs = new Map<string, RuntimeEvent>();
  private readonly receipts = new Map<string, Readonly<Record<string, unknown>>>();
  private readonly deliverablesByPath = new Map<string, Set<string>>();
  private readonly agentByThread = new Map<string, string>();
  private revision = 0;
  private dirty = false;
  private current: ConversationSnapshot = emptyConversation;

  applyAll(events: readonly RuntimeEvent[]): void {
    for (const event of events) this.apply(event);
  }

  apply(event: RuntimeEvent): void {
    const data = event.data;
    const childAgentID = this.agentByThread.get(event.thread_id);
    if (childAgentID && !event.kind.startsWith("agent.")) {
      this.applyAgentExecution(event, childAgentID);
      return;
    }
    switch (event.kind) {
      case "turn.started":
        this.ids.set(event.turn_id, event.operation_id);
        this.activeTurns.add(event.turn_id);
        this.setActivity(event.turn_id, "Thinking...");
        this.put({
          id: event.id,
          kind: "user",
          turnID: event.turn_id,
          sequence: event.sequence,
          text: stringValue(data.display_prompt ?? data.prompt),
          images: userImages(data.images)
        });
        break;
      case "turn.steered":
        {
          const outputID = this.outputByTurn.get(event.turn_id);
          const output = outputID ? this.nodes.get(outputID) : undefined;
          if (output?.kind === "assistant") {
            this.put({...output, superseded: true});
          }
        }
        this.put({
          id: event.id,
          kind: "user",
          turnID: event.turn_id,
          sequence: event.sequence,
          text: stringValue(data.prompt),
          images: [],
          steering: true
        });
        this.outputByTurn.set(
          event.turn_id,
          `output-${event.turn_id}-after-${event.id}`
        );
        break;
      case "turn.withdrawn":
        for (const node of this.nodes.values()) {
          if (node.turnID !== event.turn_id) continue;
          if (node.kind === "user") this.put({...node, withdrawn: true});
          if (node.kind === "status") this.put({
            ...node, title: "Withdrawn", text: "", failed: false,
            blocked: false, warning: false, recoverable: false, recovery: undefined
          });
        }
        this.activeTurns.delete(event.turn_id);
        this.activities.delete(event.turn_id);
        this.touch();
        break;
      case "output.delta":
        this.setActivity(event.turn_id, "Responding...");
        this.appendAssistant(event, stringValue(data.text), "");
        break;
      case "output.draft":
        this.setActivity(event.turn_id, "Responding...");
        this.appendAssistant(
          event,
          stringValue(data.text),
          stringValue(data.sample_id)
        );
        break;
      case "output.discarded":
        this.retractDraft(event, stringValue(data.sample_id));
        break;
      case "commentary.completed":
        this.addCommentary(event);
        break;
      case "reasoning.delta":
        this.setActivity(event.turn_id, "Thinking...");
        this.appendReasoning(event, stringValue(data.text));
        break;
      case "reasoning.completed":
        this.finishReasoning(event, stringValue(data.text));
        break;
      case "tool.start":
        {
          const running = this.runningTools.get(event.turn_id) ?? new Set<string>();
          running.add(stringValue(data.call_id));
          this.runningTools.set(event.turn_id, running);
          this.refreshToolActivity(event.turn_id);
        }
        this.startTool(event);
        break;
      case "tool.output":
        this.appendToolOutput(event);
        break;
      case "tool.result":
        this.finishTool(event);
        this.runningTools.get(event.turn_id)?.delete(stringValue(data.call_id));
        this.refreshToolActivity(event.turn_id);
        break;
      case "provider.attempt":
        this.applyProviderAttempt(event);
        break;
      case "command.execution":
        this.updateCommandExecution(event);
        break;
      case "approval.required":
        this.setActivity(event.turn_id, "Awaiting approval...");
        this.approvals.set(requestID(event), event);
        this.applyApproval(event);
        this.touch();
        break;
      case "approval.resolved": {
        const pending = this.approvals.get(requestID(event));
        const callID = stringValue(pending?.data.call_id);
        const node = callID ? this.toolNodeForCall(callID) : undefined;
        if (node) {
          this.put({
            ...node,
            approvalDecision: stringValue(data.decision) || "resolved"
          });
        }
        this.approvals.delete(requestID(event));
        this.refreshToolActivity(event.turn_id);
        this.touch();
        break;
      }
      case "input.required":
        this.inputs.set(requestID(event), event);
        this.refreshToolActivity(event.turn_id);
        this.touch();
        break;
      case "input.resolved":
        this.inputs.delete(requestID(event));
        this.refreshToolActivity(event.turn_id);
        this.touch();
        break;
      case "turn.completed":
        this.finishTurn(event, false);
        break;
      case "turn.failed":
        this.finishTurn(event, true);
        break;
      case "turn.canceled":
        this.finishTurn(event, true);
        break;
      case "operation.rejected":
        if (this.ids.get(event.turn_id) === event.operation_id) {
          this.finishTurn(event, true);
        }
        this.put({
          id: event.id,
          kind: "status",
          turnID: event.turn_id,
          sequence: event.sequence,
          title: "Rejected",
          text: stringValue(data.message ?? data.code ?? "Operation rejected"),
          failed: true,
          recoverable: false
        });
        break;
      case "turn.verification":
        {
          const status = stringValue(data.verdict ?? data.status);
          const label = status === "passed"
            ? "Checks passed"
            : status === "failed"
              ? "Checks failed"
              : "Not fully verified";
          const message = status === "passed"
            ? "Changed files are covered by recorded checks."
            : status === "failed"
              ? stringValue(data.message) || label
              : "No structured check covered every changed file.";
          this.put({
            id: `verification-${event.turn_id}`,
            kind: "status",
            turnID: event.turn_id,
            sequence: event.sequence,
            title: label,
            text: message,
            failed: status === "failed",
            recoverable: false
          });
        }
        break;
      case "turn.receipt":
        this.receipts.set(event.turn_id, Object.freeze({...data}));
        this.markPriorDeliverablesStale(
          event.thread_id,
          event.turn_id,
          data.changes
        );
        this.put({
          id: event.id,
          kind: "receipt",
          turnID: event.turn_id,
          sequence: event.sequence,
          data: Object.freeze({...data})
        });
        break;
      case "thread.compacted":
      case "turn.compaction":
        if (event.kind === "turn.compaction" && hideTranscriptCompaction(data)) {
          break;
        }
        this.put({
          id: event.kind === "turn.compaction"
            ? `context-${event.turn_id}`
            : event.id,
          kind: "context",
          turnID: event.turn_id,
          sequence: event.sequence,
          title: event.kind === "thread.compacted" ? "Context compacted" : "Compaction",
          summary: stringValue(data.summary ?? data.reason ?? "Context window updated"),
          data: Object.freeze({...data})
        });
        break;
      case "agent.spawned":
        this.spawnAgent(event);
        break;
      case "agent.status":
        this.updateAgentStatus(event);
        break;
      case "agent.message":
        this.applyAgentMessage(event);
        break;
      case "agent.integration":
        this.applyAgentIntegration(event);
        break;
    }
  }

  snapshot(): ConversationSnapshot {
    if (!this.dirty) return this.current;
    this.revision += 1;
    const activeTurnID = [...this.activeTurns].at(-1) ?? "";
    this.current = Object.freeze({
      order: Object.freeze([...this.order]),
      nodes: new Map(this.nodes),
      activeTurnID,
      activeStatus: this.activities.get(activeTurnID),
      pendingApproval: [...this.approvals.values()].at(-1),
      pendingInput: [...this.inputs.values()].at(-1),
      revision: this.revision
    });
    this.dirty = false;
    return this.current;
  }

  private addCommentary(event: RuntimeEvent): void {
    const id = commentaryNodeID(event);
    if (this.nodes.has(id)) return;
    const callIDs = Array.isArray(event.data.call_ids)
      ? event.data.call_ids.filter((value): value is string => typeof value === "string")
      : [];
    const anchors = callIDs.flatMap((callID) => {
      const tool = this.toolNodeForCall(callID);
      return tool && tool.turnID === event.turn_id ? [tool] : [];
    });
    const first = anchors.sort((a, b) => a.sequence - b.sequence)[0];
    this.put({
      id, kind: "commentary", turnID: event.turn_id,
      sequence: first?.sequence ?? event.sequence,
      text: stringValue(event.data.text),
      sampleID: stringValue(event.data.sample_id),
      callIDs: Object.freeze(callIDs)
    });
    // Recovery may publish a confirmed message after its tools were projected.
    if (first) {
      const index = this.order.indexOf(first.id);
      this.order.pop();
      this.order.splice(index, 0, id);
    }
  }

  private appendAssistant(
    event: RuntimeEvent,
    delta: string,
    sampleID: string
  ): void {
    const id = this.outputByTurn.get(event.turn_id) ?? `output-${event.turn_id}`;
    const segments = this.outputSegments.get(id) ?? [];
    const last = segments.at(-1);
    if (last && last.sampleID === sampleID) {
      last.text += delta;
    } else {
      segments.push({sampleID, text: delta});
    }
    this.outputSegments.set(id, segments);
    this.outputByTurn.set(event.turn_id, id);
    this.put({
      id,
      kind: "assistant",
      turnID: event.turn_id,
      sequence: event.sequence,
      text: segments.map((segment) => segment.text).join("")
    });
    // A streaming answer follows the activity it narrates. The node is born
    // with the first narration draft, which can precede the turn's tool work
    // and be retracted; without relocation the surviving text would render
    // above that work instead of below it.
    const index = this.order.indexOf(id);
    if (index >= 0 && index !== this.order.length - 1) {
      this.order.splice(index, 1);
      this.order.push(id);
      this.touch();
    }
  }

  // A draft retraction drops the named sample's streamed text. The durable
  // owners (commentary, the terminal outbox) republish whatever survives, so
  // the remaining segments keep their order. An emptied node is removed: it
  // would otherwise hover above the turn's tool work until the next sample
  // recreates it at the streaming position.
  private retractDraft(event: RuntimeEvent, sampleID: string): void {
    if (!sampleID) return;
    const id = this.outputByTurn.get(event.turn_id);
    if (!id) return;
    const segments = this.outputSegments.get(id);
    if (!segments?.some((segment) => segment.sampleID === sampleID)) return;
    const kept = segments.filter((segment) => segment.sampleID !== sampleID);
    if (kept.length === 0) {
      this.outputSegments.delete(id);
      this.remove(id);
      return;
    }
    this.outputSegments.set(id, kept);
    const previous = this.nodes.get(id);
    if (previous?.kind === "assistant") {
      this.put({...previous, text: kept.map((segment) => segment.text).join("")});
    }
  }

  private appendReasoning(event: RuntimeEvent, delta: string): void {
    const key = reasoningKey(event);
    const id = this.reasoningByTurn.get(key) ?? `reasoning-${key}`;
    const previous = this.nodes.get(id);
    const text = previous?.kind === "reasoning" ? previous.text + delta : delta;
    this.reasoningByTurn.set(key, id);
    this.put({
      id,
      kind: "reasoning",
      turnID: event.turn_id,
      sequence: previous?.sequence ?? event.sequence,
      text,
      summary: lastNonEmptyLine(text) || "Thinking",
      running: this.activeTurns.has(event.turn_id)
    });
  }

  private finishReasoning(event: RuntimeEvent, text: string): void {
    const key = reasoningKey(event);
    const currentID = this.reasoningByTurn.get(key);
    const current = currentID ? this.nodes.get(currentID) : undefined;
    const id = current?.kind === "reasoning"
      ? current.id
      : `reasoning-${event.id}`;
    this.put({
      id,
      kind: "reasoning",
      turnID: event.turn_id,
      sequence: current?.sequence ?? event.sequence,
      text,
      summary: firstNonEmptyLine(text) || "Thinking",
      running: false
    });
    this.reasoningByTurn.delete(key);
  }

  private startTool(event: RuntimeEvent): void {
    const tool = stringValue(event.data.tool) || "Tool";
    if (tool === "turn_complete" || tool === "request_user_input") return;
    const callID = stringValue(event.data.call_id) || event.id;
    const id = `tool-${callID}`;
    const args = event.data.arguments;
    const editPlan = editPlanFromArguments(tool, args);
    this.ids.set(callID, id);
    this.put({
      id,
      kind: "tool",
      turnID: event.turn_id,
      sequence: event.sequence,
      callID,
      tool,
      variant: editPlan ? "diff" : toolVariant(tool, event.data),
      title: toolTitle(tool),
      summary: toolSummary(tool, args),
      state: "running",
      arguments: args,
      output: "",
      errorSummary: "",
      truncated: false,
      changes: [],
      ...(editPlan ? {editPlan} : {})
    });
  }

  private appendToolOutput(event: RuntimeEvent): void {
    const node = this.toolNode(event);
    if (!node) return;
    this.put({
      ...node,
      output: node.output + stringValue(event.data.chunk)
    });
  }

  private finishTool(event: RuntimeEvent): void {
    const node = this.toolNode(event);
    if (!node) return;
    const output = stringValue(event.data.output) || node.output;
    const failed = Boolean(event.data.is_error);
    if (failed && isPlanGateRetry(event.data.recovery)) {
      this.remove(node.id);
      return;
    }
    const changes = Array.isArray(event.data.changes)
      ? event.data.changes.filter(isRecord)
      : [];
    const variant = node.variant === "shell"
      ? node.variant
      : changes.length > 0
        ? "diff"
        : node.variant;
    this.put({
      ...node,
      variant,
      summary: failed
        ? firstNonEmptyLine(output) || "Tool failed"
        : changes.length > 0 && node.variant !== "shell"
          ? changeSummary(changes)
          : node.summary,
      state: failed ? "failed" : "completed",
      output,
      errorSummary: failed ? firstNonEmptyLine(output) || "Tool failed" : "",
      execution: isRecord(event.data.execution) ? event.data.execution : undefined,
      truncated: Boolean(event.data.truncated),
      changes,
      recovery: isRecord(event.data.recovery) ? event.data.recovery : undefined,
      contextText: output || undefined
    });
  }

  private applyApproval(event: RuntimeEvent): void {
    const callID = stringValue(event.data.call_id);
    const node = this.toolNodeForCall(callID);
    if (!node) return;
    const editPlan = projectEditPlan(event.data.edit_plan);
    this.put({
      ...node,
      variant: editPlan ? "diff" : node.variant,
      ...(editPlan ? {editPlan} : {})
    });
  }

  private toolNodeForCall(callID: string) {
    const id = this.ids.get(callID);
    const node = id ? this.nodes.get(id) : undefined;
    return node?.kind === "tool" ? node : undefined;
  }

  private updateCommandExecution(event: RuntimeEvent): void {
    const node = this.toolNode(event);
    if (!node) return;
    const exitCode = numberValue(event.data.exit_code);
    const durationMS = numberValue(event.data.duration_ms);
    const failed = stringValue(event.data.status) === "failed" ||
      (exitCode !== undefined && exitCode !== 0);
    this.put({
      ...node,
      errorSummary: failed
        ? stringValue(event.data.command)
        : node.errorSummary,
      command: {
        command: stringValue(event.data.command),
        status: stringValue(event.data.status),
        ...(exitCode === undefined ? {} : {exitCode}),
        ...(durationMS === undefined ? {} : {durationMS})
      }
    });
  }

  private spawnAgent(event: RuntimeEvent): void {
    const agentID = stringValue(event.data.agent_id);
    if (!agentID) return;
    const detail = recordValue(event.data.detail);
    const threadID = stringValue(detail?.thread_id);
    if (threadID) this.agentByThread.set(threadID, agentID);
    const taskName = stringValue(detail?.task_name);
    this.put({
      id: agentNodeID(agentID),
      kind: "agent",
      turnID: event.turn_id,
      sequence: event.sequence,
      agentID,
      role: stringValue(event.data.role) || "agent",
      taskName,
      status: "requested",
      summary: taskName || "Waiting to start",
      state: "running",
      activities: Object.freeze([agentStartupActivity(
        event,
        agentID,
        "Queued",
        taskName || "Agent accepted the delegated task.",
        "running"
      )])
    });
  }

  private updateAgentStatus(event: RuntimeEvent): void {
    const agentID = stringValue(event.data.agent_id);
    const node = this.agentNode(agentID);
    if (!node) return;
    const status = stringValue(event.data.status) || node.status;
    const message = stringValue(event.data.message);
    const reasonCode = stringValue(event.data.reason_code);
    const detail = recordValue(event.data.detail);
    const result = recordValue(detail?.result);
    const usage = agentUsage(result);
    const state = agentState(status);
    const activity = agentStartupStatus(status)
      ? agentStartupActivity(
          event,
          agentID,
          agentStatusLabel(status),
          node.taskName || message || agentStatusLabel(status),
          "running",
          node.activities
        )
      : agentActivity(
          event,
          "status",
          agentStatusLabel(status),
          message || reasonCode,
          state
        );
    this.put({
      ...node,
      status,
      state,
      reasonCode: reasonCode || node.reasonCode,
      usage: usage ?? node.usage,
      summary: message || reasonCode || agentStatusLabel(status),
      activities: appendAgentActivity(node.activities, activity)
    });
  }

  private applyAgentMessage(event: RuntimeEvent): void {
    const message = recordValue(event.data.body);
    const agentID = stringValue(message?.from);
    const node = this.agentNode(agentID);
    if (!node) return;
    const body = recordValue(message?.body);
    const kind = stringValue(message?.kind);
    if (kind === "context") return;
    const resultSummary = stringValue(body?.summary);
    const activitySummary = resultSummary ||
      (kind === "completion" ? "Completion delivered" : "Message delivered");
    this.put({
      ...node,
      summary: resultSummary || node.summary,
      activities: appendAgentActivity(node.activities, agentActivity(
        event,
        "message",
        kind === "completion" ? "Result" : "Message",
        activitySummary,
        kind === "completion" ? "completed" : node.state
      ))
    });
  }

  private applyAgentIntegration(event: RuntimeEvent): void {
    const agentID = stringValue(event.data.agent_id);
    const node = this.agentNode(agentID);
    if (!node) return;
    const status = stringValue(event.data.status);
    const message = stringValue(event.data.message);
    this.put({
      ...node,
      summary: message || `Integration ${status || "updated"}`,
      activities: appendAgentActivity(node.activities, agentActivity(
        event,
        "status",
        "Integration",
        message || status,
        status === "failed" || status === "conflicted" ? "failed" : "completed"
      ))
    });
  }

  private applyAgentExecution(event: RuntimeEvent, agentID: string): void {
    const node = this.agentNode(agentID);
    if (!node) return;
    const data = event.data;
    switch (event.kind) {
      case "commentary.completed": {
        const id = commentaryNodeID(event);
        if (node.activities.some((item) => item.id === id)) break;
        const callIDs = Array.isArray(data.call_ids) ? data.call_ids : [];
        const anchor = node.activities.find((item) =>
          item.callID && callIDs.includes(item.callID)
        );
        const activity: ProjectedAgentActivity = Object.freeze({
          id, sequence: anchor?.sequence ?? event.sequence,
          kind: "message", title: "Update",
          summary: stringValue(data.text), state: "completed"
        });
        const activities = [...node.activities];
        const index = anchor ? activities.indexOf(anchor) : activities.length;
        activities.splice(index, 0, activity);
        this.put({
          ...node,
          summary: anchor || node.state !== "running" ? node.summary : activity.summary,
          activities: Object.freeze(activities)
        });
        break;
      }
      case "turn.started":
        this.updateAgent(node, agentStartupActivity(
          event,
          agentID,
          "Started",
          node.taskName || "Agent turn started",
          "completed",
          node.activities
        ), "running");
        break;
      case "reasoning.delta":
      case "reasoning.completed": {
        const id = `agent-reasoning-${event.turn_id}-${
          stringValue(data.sample_id) || "active"
        }`;
        const previous = node.activities.find((item) => item.id === id);
        const text = stringValue(data.text);
        const activity: ProjectedAgentActivity = Object.freeze({
          id,
          sequence: previous?.sequence ?? event.sequence,
          kind: "reasoning",
          title: "Thinking",
          summary: lastNonEmptyLine(text) || previous?.summary || "Thinking",
          state: event.kind === "reasoning.completed" ? "completed" : "running",
        });
        this.updateAgent(node, activity, "running");
        break;
      }
      case "tool.start": {
        const tool = stringValue(data.tool) || "Tool";
        const callID = stringValue(data.call_id) || event.id;
        this.updateAgent(node, Object.freeze({
          id: `agent-tool-${callID}`,
          sequence: event.sequence,
          kind: "tool",
          title: toolTitle(tool),
          summary: toolSummary(tool, data.arguments),
          state: "running",
          callID
        }), "running");
        break;
      }
      case "tool.result": {
        const callID = stringValue(data.call_id);
        const id = `agent-tool-${callID}`;
        const previous = node.activities.find((item) => item.id === id);
        if (!previous) break;
        const output = stringValue(data.output);
        const failed = Boolean(data.is_error);
        this.updateAgent(node, Object.freeze({
          ...previous,
          summary: failed
            ? firstNonEmptyLine(output) || "Tool failed"
            : previous.summary,
          state: failed ? "failed" : "completed",
          callID
        }), "running");
        break;
      }
      case "output.delta": {
        const id = `agent-output-${event.turn_id}`;
        const previous = node.activities.find((item) => item.id === id);
        const text = stringValue(data.text);
        this.updateAgent(node, Object.freeze({
          id,
          sequence: previous?.sequence ?? event.sequence,
          kind: "message",
          title: "Response",
          summary: lastNonEmptyLine(text) || previous?.summary || "Responding",
          state: "running"
        }), "running");
        break;
      }
      case "turn.completed": {
        const text = stringValue(data.text ?? data.summary);
        const id = `agent-output-${event.turn_id}`;
        const previous = node.activities.find((item) => item.id === id);
        this.updateAgent(node, Object.freeze({
          id,
          sequence: previous?.sequence ?? event.sequence,
          kind: "message",
          title: "Result",
          summary: firstNonEmptyLine(text) || "Completed",
          state: "completed"
        }), "completed");
        break;
      }
      case "turn.failed":
      case "turn.canceled": {
        const summary = stringValue(data.message ?? data.reason) ||
          (event.kind === "turn.canceled" ? "Canceled" : "Failed");
        this.updateAgent(node, agentActivity(
          event,
          "status",
          event.kind === "turn.canceled" ? "Canceled" : "Failed",
          summary,
          "failed"
        ), "failed");
        break;
      }
    }
  }

  private updateAgent(
    node: Extract<ConversationNode, {kind: "agent"}>,
    activity: ProjectedAgentActivity,
    state: "running" | "completed" | "failed"
  ): void {
    this.put({
      ...node,
      state,
      summary: activity.summary || node.summary,
      activities: appendAgentActivity(node.activities, activity)
    });
  }

  private agentNode(agentID: string) {
    const node = this.nodes.get(agentNodeID(agentID));
    return node?.kind === "agent" ? node : undefined;
  }

  private applyProviderAttempt(event: RuntimeEvent): void {
    const presentation = providerAttemptPresentation(event.data);
    if (!presentation) {
      if (event.data.status === "started" || event.data.status === "completed") {
        this.remove(providerStateID(event.turn_id));
        this.setActivity(event.turn_id, event.data.stop_reason === "tool_use"
          ? "Preparing tools..." : "Thinking...");
      }
      return;
    }
    this.setActivity(event.turn_id, presentation.title);
    this.put({
      id: providerStateID(event.turn_id),
      kind: "status",
      turnID: event.turn_id,
      sequence: event.sequence,
      title: presentation.title,
      text: presentation.text,
      failed: false,
      warning: presentation.warning,
      recoverable: false
    });
  }

  private setActivity(turnID: string, status: string): void {
    if (this.activities.get(turnID) === status) return;
    this.activities.set(turnID, status);
    this.touch();
  }

  private refreshToolActivity(turnID: string): void {
    for (const event of this.approvals.values()) {
      if (event.turn_id === turnID) {
        this.setActivity(turnID, "Awaiting approval...");
        return;
      }
    }
    for (const event of this.inputs.values()) {
      if (event.turn_id === turnID) {
        this.setActivity(turnID, "Waiting for input...");
        return;
      }
    }
    this.setActivity(turnID, this.runningTools.get(turnID)?.size
      ? "Running tools..." : "Continuing...");
  }

  private finishTurn(event: RuntimeEvent, failed: boolean): void {
    this.remove(providerStateID(event.turn_id));
    this.activeTurns.delete(event.turn_id);
    this.activities.delete(event.turn_id);
    this.runningTools.delete(event.turn_id);
    const settledOutput = this.outputByTurn.get(event.turn_id);
    if (settledOutput) this.outputSegments.delete(settledOutput);
    for (const [key, pending] of this.approvals) {
      if (pending.turn_id === event.turn_id) this.approvals.delete(key);
    }
    for (const [key, pending] of this.inputs) {
      if (pending.turn_id === event.turn_id) this.inputs.delete(key);
    }
    for (const [key, reasoningID] of this.reasoningByTurn) {
      const reasoning = this.nodes.get(reasoningID);
      if (reasoning?.kind !== "reasoning" ||
          reasoning.turnID !== event.turn_id) {
        continue;
      }
      this.put({...reasoning, running: false});
      this.reasoningByTurn.delete(key);
    }
    if (event.kind === "turn.completed") {
      const finalText = stringValue(event.data.text ?? event.data.summary);
      const outputID = this.outputByTurn.get(event.turn_id);
      const output = outputID ? this.nodes.get(outputID) : undefined;
      if (output?.kind === "assistant" && finalText) {
        this.put({...output, text: finalText});
      } else if (finalText) {
        const id = `output-${event.turn_id}`;
        this.outputByTurn.set(event.turn_id, id);
        this.put({
          id,
          kind: "assistant",
          turnID: event.turn_id,
          sequence: event.sequence,
          text: finalText
        });
      }
      this.putDeliverables(event);
      this.touch();
      return;
    }
    const failure = failurePresentation(event);
    const blocked = failure.blocked === true;
    const warning = failure.warning === true;
    this.put({
      id: `${event.id}-status`,
      kind: "status",
      turnID: event.turn_id,
      sequence: event.sequence,
      title: failure.title,
      text: failure.text,
      failed: failed && !warning,
      blocked,
      warning,
      recoverable: failed,
      recovery: failed ? recoveryOptions(
        event,
        this.receipts.get(event.turn_id)
      ) : undefined
    });
    this.putDeliverables(event);
  }

  private markPriorDeliverablesStale(
    threadID: string,
    turnID: string,
    value: unknown
  ): void {
    if (!Array.isArray(value)) return;
    for (const change of value) {
      if (!isRecord(change)) continue;
      const path = stringValue(change.path);
      if (!path) continue;
      for (const id of this.deliverablesByPath.get(
        deliverablePathKey(threadID, path)
      ) ?? []) {
        const node = this.nodes.get(id);
        if (node?.kind !== "deliverables" || node.turnID === turnID) continue;
        this.put({
          ...node,
          files: Object.freeze(node.files.map((file) =>
            file.path === path ? Object.freeze({...file, stale: true}) : file
          ))
        });
      }
    }
  }

  private putDeliverables(event: RuntimeEvent): void {
    const receipt = this.receipts.get(event.turn_id);
    if (!receipt || !Array.isArray(receipt.changes) || receipt.changes.length === 0) {
      return;
    }
    const id = `deliverables-${event.turn_id}`;
    const files = receipt.changes.flatMap((change) => {
      if (!isRecord(change)) return [];
      const path = stringValue(change.path);
      if (!path) return [];
      const tool = this.toolForChange(event.turn_id, path, stringValue(change.tool));
      return [Object.freeze({
        path,
        tool: stringValue(change.tool) || tool?.tool || "workspace",
        kind: stringValue(change.kind) || "modified",
        added: numberValue(change.added) ?? 0,
        removed: numberValue(change.removed) ?? 0,
        summary: stringValue(change.summary),
        callID: tool?.callID,
        diff: tool?.editPlan?.files.find((file) => file.path === path),
        stale: false
      })];
    });
    if (files.length === 0) return;
    this.put({
      id,
      kind: "deliverables",
      turnID: event.turn_id,
      sequence: event.sequence,
      files: Object.freeze(files),
      verification: verificationState(receipt.verification)
    });
    for (const file of files) {
      const key = deliverablePathKey(event.thread_id, file.path);
      const ids = this.deliverablesByPath.get(key) ?? new Set<string>();
      ids.add(id);
      this.deliverablesByPath.set(key, ids);
    }
  }

  private toolForChange(turnID: string, path: string, tool: string) {
    return [...this.nodes.values()].reverse().find((node) =>
      node.kind === "tool" &&
      node.turnID === turnID &&
      (!tool || node.tool === tool) &&
      (
        node.changes.some((change) => stringValue(change.path) === path) ||
        node.editPlan?.files.some((file) => file.path === path)
      )
    ) as Extract<ConversationNode, {kind: "tool"}> | undefined;
  }

  private toolNode(event: RuntimeEvent): Extract<ConversationNode, {kind: "tool"}> | undefined {
    const callID = stringValue(event.data.call_id);
    const id = this.ids.get(callID);
    const node = id ? this.nodes.get(id) : undefined;
    return node?.kind === "tool" ? node : undefined;
  }

  private put(node: ConversationNode): void {
    if (!this.nodes.has(node.id)) this.order.push(node.id);
    this.nodes.set(node.id, Object.freeze(node));
    this.touch();
  }

  private remove(id: string): void {
    if (!this.nodes.delete(id)) return;
    const index = this.order.indexOf(id);
    if (index >= 0) this.order.splice(index, 1);
    this.touch();
  }

  private touch(): void {
    this.dirty = true;
  }
}

function failurePresentation(event: RuntimeEvent) {
  const fallback = stringValue(
    event.data.outcome ??
    event.data.message ??
    event.data.reason ??
    "Turn did not complete"
  );
  const budget = fallback.match(
    /^token budget exhausted: projected (\d+), limit (\d+)$/
  );
  if (budget) {
    return {
      title: "Token limit reached",
      text: `The next model call would exceed this run's token limit (${formatInteger(
        budget[1]
      )} projected, ${formatInteger(budget[2])} allowed).`
    };
  }
  const convergence = isRecord(event.data.convergence)
    ? event.data.convergence
    : undefined;
  if (stringValue(convergence?.cause)) {
    return {
      title: "Blocked",
      text: stringValue(convergence?.summary) || fallback,
      blocked: true,
      warning: true
    };
  }
  const fault = isRecord(event.data.fault) ? event.data.fault : undefined;
  if (["retry_step", "retry_turn", "resume_turn"].includes(
    stringValue(fault?.disposition)
  )) {
    return {
      title: "Blocked",
      text: fallback,
      blocked: true,
      warning: true
    };
  }
  if (
    event.kind === "turn.canceled" &&
    stringValue(event.data.reason) === "user_interrupted"
  ) {
    return {
      title: "Paused",
      text: "Paused by user.",
      warning: true
    };
  }
  return {
    title: event.kind === "turn.canceled" ? "Canceled" : "Failed",
    text: fallback
  };
}

function formatInteger(value: string): string {
  return value.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

function requestID(event: RuntimeEvent): string {
  return stringValue(event.data.request_id) || event.id;
}

function reasoningKey(event: RuntimeEvent): string {
  const sampleID = stringValue(event.data.sample_id);
  return `${event.turn_id}:${sampleID || "active"}`;
}

export function commentaryNodeID(event: RuntimeEvent): string {
  return `commentary-${event.turn_id}:${stringValue(event.data.message_id) || event.id}`;
}

function deliverablePathKey(threadID: string, path: string): string {
  return `${threadID}\u0000${path}`;
}

function stringValue(value: unknown): string {
  return value === undefined || value === null ? "" : String(value);
}

function recordValue(value: unknown): Record<string, unknown> | undefined {
  if (isRecord(value)) return value;
  if (typeof value !== "string") return undefined;
  try {
    const parsed: unknown = JSON.parse(value);
    return isRecord(parsed) ? parsed : undefined;
  } catch {
    return undefined;
  }
}

function agentNodeID(agentID: string): string {
  return `agent-${agentID}`;
}

function agentState(status: string): "running" | "completed" | "failed" {
  switch (status) {
    case "completed":
    case "closed":
      return "completed";
    case "failed":
    case "errored":
    case "interrupted":
    case "shutdown":
      return "failed";
    default:
      return "running";
  }
}

function agentUsage(
  result: Record<string, unknown> | undefined
): {readonly inputTokens: number; readonly outputTokens: number} | undefined {
  const usage = recordValue(result?.usage);
  if (!usage) return undefined;
  const inputTokens = Number(usage.input_tokens ?? 0);
  const outputTokens = Number(usage.output_tokens ?? 0);
  if (!Number.isFinite(inputTokens) && !Number.isFinite(outputTokens)) {
    return undefined;
  }
  return {inputTokens, outputTokens};
}

function agentStatusLabel(status: string): string {
  return status
    ? status.replaceAll("_", " ").replace(/\b\w/g, (value) => value.toUpperCase())
    : "Updated";
}

function agentStartupStatus(status: string): boolean {
  return status === "requested" || status === "starting" || status === "running";
}

function agentStartupActivity(
  event: RuntimeEvent,
  agentID: string,
  title: string,
  summary: string,
  state: ProjectedAgentActivity["state"],
  activities: readonly ProjectedAgentActivity[] = []
): ProjectedAgentActivity {
  const id = `agent-startup-${agentID}`;
  const previous = activities.find((activity) => activity.id === id);
  return Object.freeze({
    id,
    sequence: previous?.sequence ?? event.sequence,
    kind: "status",
    title,
    summary,
    state
  });
}

function agentActivity(
  event: RuntimeEvent,
  kind: ProjectedAgentActivity["kind"],
  title: string,
  summary: string,
  state: ProjectedAgentActivity["state"]
): ProjectedAgentActivity {
  return Object.freeze({
    id: `agent-activity-${event.id}`,
    sequence: event.sequence,
    kind,
    title,
    summary,
    state
  });
}

function appendAgentActivity(
  activities: readonly ProjectedAgentActivity[],
  activity: ProjectedAgentActivity
): readonly ProjectedAgentActivity[] {
  const index = activities.findIndex((item) => item.id === activity.id);
  if (index < 0) return Object.freeze([...activities, activity]);
  const next = [...activities];
  next[index] = activity;
  return Object.freeze(next);
}

function isPlanGateRetry(value: unknown): boolean {
  if (!isRecord(value)) return false;
  return value.error_category === "plan_required" &&
    value.required_action === "submit_plan" &&
    value.retry_original === false;
}

function userImages(value: unknown): readonly ProjectedUserImage[] {
  if (!Array.isArray(value)) return [];
  return Object.freeze(value.flatMap((item) => {
    if (!isRecord(item)) return [];
    const label = stringValue(item.label);
    const mediaType = stringValue(item.media_type);
    const content = stringValue(item.content);
    if (!label || !/^image\/(png|jpeg|gif|webp)$/.test(mediaType) || !content) {
      return [];
    }
    return [Object.freeze({label, mediaType, content})];
  }));
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : undefined;
}

function hideTranscriptCompaction(data: Record<string, unknown>): boolean {
  const removedMessages = numberValue(data.removed_messages) ?? 0;
  const prunedResults = numberValue(data.pruned_tool_results) ?? 0;
  return stringValue(data.phase) === "post_turn" ||
    (stringValue(data.status) === "fallback" &&
      removedMessages === 0 &&
      prunedResults === 0);
}

function firstNonEmptyLine(value: string): string {
  return value.split(/\r?\n/).map((line) => line.trim()).find(Boolean) ?? "";
}

function lastNonEmptyLine(value: string): string {
  const lines = value.split(/\r?\n/);
  for (let index = lines.length - 1; index >= 0; index -= 1) {
    const line = lines[index]?.trim();
    if (line) return line;
  }
  return "";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function recoveryOptions(
  event: RuntimeEvent,
  receipt: Readonly<Record<string, unknown>> | undefined
) {
  const fault = isRecord(event.data.fault) ? event.data.fault : undefined;
  const disposition = stringValue(fault?.disposition);
  let sideEffects = stringValue(fault?.side_effects) || "unknown";
  if ((sideEffects === "unknown" || !sideEffects) && receipt) {
    const changes = Array.isArray(receipt.changes) ? receipt.changes : [];
    const workspace = isRecord(receipt.workspace_outcome)
      ? receipt.workspace_outcome
      : undefined;
    if (changes.length > 0 || workspace?.changed === true) {
      sideEffects = "committed";
    }
  }
  const legacy = !disposition;
  return Object.freeze({
    canRetry: event.kind === "turn.canceled" ||
      disposition === "retry_step" ||
      disposition === "retry_turn" ||
      legacy,
    canContinue: event.kind === "turn.canceled" ||
      disposition === "retry_step" ||
      disposition === "resume_turn" ||
      disposition === "retry_turn" ||
      disposition === "fail_turn" ||
      legacy,
    sideEffects,
    action: stringValue(fault?.recovery_action)
  });
}

function verificationState(
  value: unknown
): "passed" | "failed" | "unverified" {
  if (!isRecord(value)) return "unverified";
  const states = ["diagnostics", "tests", "verify"]
    .map((key) => stringValue(value[key]))
    .filter(Boolean);
  if (states.includes("failed")) return "failed";
  return states.includes("passed") ? "passed" : "unverified";
}

export function projectEditPlan(value: unknown): ProjectedEditPlan | undefined {
  if (!isRecord(value) || !Array.isArray(value.files)) return undefined;
  const files = value.files.flatMap((entry) => {
    if (!isRecord(entry) || typeof entry.path !== "string") return [];
    const operation = stringValue(entry.op);
    return [{
      path: entry.path,
      kind: stringValue(entry.kind) ||
        (operation === "create" ? "created" : operation === "delete" ? "deleted" : "modified"),
      before: stringValue(entry.before ?? entry.old),
      after: stringValue(entry.after ?? entry.new),
      beforeExists: entry.before_exists === undefined
        ? operation !== "create"
        : Boolean(entry.before_exists),
      afterExists: entry.after_exists === undefined
        ? operation !== "delete"
        : Boolean(entry.after_exists)
    }];
  });
  if (files.length === 0) return undefined;
  return Object.freeze({
    id: stringValue(value.id),
    diff: stringValue(value.diff),
    files: Object.freeze(files)
  });
}

function editPlanFromArguments(
  tool: string,
  value: unknown
): ProjectedEditPlan | undefined {
  const input = isRecord(value) ? value : undefined;
  if (tool === "file_apply") {
    if (!Array.isArray(input?.changes) || input.changes.some((entry) =>
      !isRecord(entry) || entry.op !== "edit"
    )) {
      return undefined;
    }
    return projectEditPlan({
      files: input.changes.map((entry) => ({
        path: entry.path,
        kind: "modified",
        before: entry.old,
        after: entry.new,
        before_exists: true,
        after_exists: true
      }))
    });
  }
  const write = tool.includes("write");
  if (!write && tool !== "file_edit" && tool !== "edit_file") {
    return undefined;
  }
  const path = stringValue(input?.path);
  const before = stringValue(input?.old);
  const after = stringValue(write ? input?.content : input?.new);
  if (!path || !after || before === after) return undefined;
  return Object.freeze({
    id: "",
    diff: "",
    files: Object.freeze([{
      path,
      kind: "modified",
      before,
      after,
      beforeExists: !write,
      afterExists: true
    }])
  });
}

function toolVariant(tool: string, data: Record<string, unknown>): ToolVariant {
  if (Array.isArray(data.changes) && data.changes.length > 0) return "diff";
  switch (tool) {
    case "file_read":
    case "result_get":
    case "read":
    case "read_file":
      return "read";
    case "text_search":
    case "search_text":
    case "search_project":
    case "search_files":
    case "file_list":
    case "file_search":
    case "symbol_search":
    case "grep":
    case "glob":
      return "search";
    case "shell_read":
    case "exec_command":
    case "shell":
      return "shell";
    case "file_write":
    case "write_file":
    case "file_edit":
    case "edit_file":
    case "apply_patch":
      return "write";
  }
  if (tool.includes("agent")) return "agent";
  return "generic";
}

function toolTitle(tool: string): string {
  switch (tool) {
    case "file_read":
    case "read":
    case "read_file":
      return "Read";
    case "exec_command":
    case "shell":
    case "shell_read":
      return "Bash";
    case "text_search":
    case "search_text":
    case "search_project":
    case "grep":
      return "Grep";
    case "file_search":
    case "search_files":
    case "file_list":
    case "glob":
      return "Glob";
    case "file_edit":
    case "edit_file":
    case "file_apply":
    case "file_patch":
    case "apply_patch":
      return "Edit";
    case "file_write":
    case "write_file":
      return "Write";
  }
  return tool
    .replaceAll("_", " ")
    .replace(/\b\w/g, (value) => value.toUpperCase());
}

function toolSummary(tool: string, value: unknown): string {
  const args = isRecord(value) ? value : {};
  if (tool === "result_get") {
    return `Read full result · ${readableArg(args, ["handle", "result_id", "id"]) || "result handle"}`;
  }
  if (["text_search", "search_text", "search_project", "grep"].includes(tool)) {
    const expression = readableArg(args, ["query", "pattern"]);
    const path = readableArg(args, ["path", "cwd", "root"]);
    if (expression && path) return `${expression} · ${path}`;
    if (expression || path) return expression || path;
  }
  const summary = readableArg(args, [
    "description", "path", "file_path", "query", "pattern", "cmd", "command",
    "symbol", "name", "role", "task"
  ]);
  if (summary) return summary.split(/\r?\n/, 1)[0]!;
  const values = Object.values(args);
  const first = values.find((entry) =>
    typeof entry === "string" || typeof entry === "number"
  );
  return first === undefined ? "Waiting for result" : String(first);
}

function readableArg(
  args: Record<string, unknown>,
  keys: readonly string[]
): string {
  for (const key of keys) {
    const value = args[key];
    if (typeof value === "string" && value.trim()) return value.trim();
    if (typeof value === "number") return String(value);
  }
  return "";
}

function changeSummary(changes: readonly Record<string, unknown>[]): string {
  let added = 0;
  let removed = 0;
  for (const change of changes) {
    added += Number(change.added ?? change.added_lines ?? 0);
    removed += Number(change.removed ?? change.removed_lines ?? 0);
  }
  return `${stringValue(changes[0]?.path)} · +${added} -${removed}`;
}

function providerStateID(turnID: string): string {
  return `provider-state-${turnID}`;
}

function providerAttemptPresentation(data: Record<string, unknown>): {
  title: string;
  text: string;
  warning?: boolean;
} | undefined {
  const status = stringValue(data.status);
  const failure = stringValue(data.failure_code);
  if (status === "retry_wait" && failure === "rate_limit") {
    return {
      title: "Provider rate limited",
      text: "Waiting to retry. This is a request limit, not a truncated message.",
      warning: true
    };
  }
  if (status === "retry_wait") {
    return {
      title: "Provider retrying",
      text: "Waiting to retry a failed provider request.",
      warning: true
    };
  }
  if (status === "incomplete") {
    return {
      title: "Provider output incomplete",
      text: "Safely continuing from the confirmed output.",
      warning: true
    };
  }
  return undefined;
}
