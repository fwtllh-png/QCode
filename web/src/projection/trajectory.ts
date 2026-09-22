import type {
  RuntimeEvent,
  TraceSnapshot,
  TraceSpan,
  TraceSpanKind
} from "../protocol";
import {commentaryNodeID} from "./conversation";

export type TrajectoryKind =
  | "system"
  | "user"
  | "context"
  | "assistant"
  | "tool"
  | "verification"
  | "receipt"
  | "unknown";

export type TrajectoryLane = "input" | "model" | "tools";

export interface TrajectoryRecord {
  readonly id: string;
  readonly sequence: number;
  readonly createdAt: string;
  readonly turnID: string;
  readonly itemID: string;
  readonly callID: string;
  readonly kind: TrajectoryKind;
  readonly label: string;
  readonly summary: string;
  readonly input?: unknown;
  readonly output?: unknown;
  readonly usage?: Readonly<Record<string, unknown>>;
  readonly timing?: Readonly<Record<string, unknown>>;
  readonly changes?: readonly unknown[];
  readonly raw: Readonly<Record<string, unknown>>;
  readonly searchText: string;
  readonly failed: boolean;
}

export interface TrajectorySpan {
  readonly id: string;
  readonly recordID: string;
  readonly turnID: string;
  readonly lane: TrajectoryLane;
  readonly kind: string;
  readonly status: string;
  readonly startedAt: number;
  readonly endedAt?: number;
  readonly durationMS?: number;
  readonly ttftMS?: number;
  readonly title: string;
}

export interface TrajectoryProjection {
  readonly records: readonly TrajectoryRecord[];
  readonly spans: readonly TrajectorySpan[];
  readonly prefixTokens?: number;
}

export function projectTrajectory(
  events: readonly RuntimeEvent[],
  trace?: TraceSnapshot
): TrajectoryProjection {
  const records: TrajectoryRecord[] = [];
  const recordIndex = new Map<string, number>();
  const assistantByTurn = new Map<string, string>();
  const reasoningByTurn = new Map<string, string>();
  const toolByCall = new Map<string, string>();
  const prefixes = new Map<string, number>();

  const put = (record: TrajectoryRecord) => {
    const index = recordIndex.get(record.id);
    if (index === undefined) {
      recordIndex.set(record.id, records.length);
      records.push(Object.freeze(record));
    } else {
      records[index] = Object.freeze(record);
    }
  };

  for (const event of events) {
    const data = event.data;
    switch (event.kind) {
      case "turn.started":
        put(record(event, "user", "USER", summary(
          data.display_prompt ?? data.prompt
        ), {input: data.display_prompt ?? data.prompt}));
        break;
      case "output.delta":
        appendTextRecord(
          event, "assistant", "ASSISTANT", assistantByTurn, records, recordIndex,
          put
        );
        break;
      // output.draft is optimistic, transient sample text that the contract
      // retracts via output.discarded; the durable output.delta projection
      // above already builds the model lane. Rendering either would spam one
      // UNKNOWN row per streamed chunk.
      case "output.draft":
      case "output.discarded":
        break;
      case "turn.queued":
      case "turn.queue.updated":
      case "turn.queue.removed":
      case "turn.withdrawn":
      case "session.title.updated":
        put(record(event, "system", "SYSTEM", eventSummary(event), {
          output: data
        }));
        break;
      case "commentary.completed": {
        const id = commentaryNodeID(event);
        if (!recordIndex.has(id)) {
          put(record(event, "assistant", "UPDATE", summary(data.text), {
            id, output: data.text
          }));
        }
        break;
      }
      case "reasoning.delta":
        appendReasoningRecord(
          event, reasoningByTurn, records, recordIndex, put, false
        );
        break;
      case "reasoning.completed":
        appendReasoningRecord(
          event, reasoningByTurn, records, recordIndex, put, true
        );
        break;
      case "tool.start": {
        if (data.tool === "turn_complete" || data.tool === "request_user_input") {
          break;
        }
        const callID = text(data.call_id) || event.id;
        const id = `tool-${callID}`;
        toolByCall.set(callID, id);
        put(record(
          event,
          "tool",
          "TOOL",
          `${text(data.tool) || "Tool"} · ${argumentSummary(data.arguments)}`,
          {
            id,
            callID,
            input: data.arguments,
            output: undefined
          }
        ));
        break;
      }
      case "tool.output": {
        const callID = text(data.call_id);
        const id = toolByCall.get(callID);
        const index = id === undefined ? undefined : recordIndex.get(id);
        const previous = index === undefined ? undefined : records[index];
        if (previous) {
          put({
            ...previous,
            output: text(previous.output) + text(data.chunk),
            raw: Object.freeze({...previous.raw, output: text(previous.output) + text(data.chunk)})
          });
        }
        break;
      }
      case "tool.result": {
        if (data.tool === "turn_complete" || data.tool === "request_user_input") {
          break;
        }
        const callID = text(data.call_id);
        const id = toolByCall.get(callID);
        const index = id === undefined ? undefined : recordIndex.get(id);
        const previous = index === undefined ? undefined : records[index];
        const failed = Boolean(data.is_error);
        if (previous) {
          const output = data.output ?? previous.output;
          put({
            ...previous,
            summary: failed
              ? `${text(data.tool) || "Tool"} · ${firstLine(output) || "Failed"}`
              : `${text(data.tool) || previous.summary.split(" · ")[0]} · ` +
                `${argumentSummary(previous.input)} -> ${firstLine(output) || "Done"}`,
            output,
            timing: isRecord(data.execution) ? data.execution : undefined,
            changes: Array.isArray(data.changes) ? data.changes : undefined,
            raw: Object.freeze({...data}),
            searchText: `${previous.searchText} ${searchable(data)}`,
            failed
          });
        } else {
          put(record(
            event,
            "tool",
            "TOOL",
            `${text(data.tool) || "Tool"} · ${firstLine(data.output) || "Result"}`,
            {
              id: `tool-${callID || event.id}`,
              callID,
              output: data.output,
              timing: isRecord(data.execution) ? data.execution : undefined,
              changes: Array.isArray(data.changes) ? data.changes : undefined,
              failed
            }
          ));
        }
        break;
      }
      case "turn.verification":
        put(record(
          event,
          "verification",
          "VERIFY",
          summary(data.verdict ?? data.status ?? "Verification"),
          {output: data, failed: data.verdict === "failed" || data.status === "failed"}
        ));
        break;
      case "turn.receipt":
        put(record(event, "receipt", "RECEIPT", receiptSummary(data), {
          output: data.outcome,
          usage: data,
          timing: isRecord(data.latency) ? data.latency : undefined,
          changes: Array.isArray(data.changes) ? data.changes : undefined
        }));
        break;
      case "turn.compaction": {
        const removedMessages = finiteNumber(data.removed_messages) ?? 0;
        const prunedResults = finiteNumber(data.pruned_tool_results) ?? 0;
        if (removedMessages === 0 && prunedResults === 0) break;
        put(record(event, "context", removedMessages > 0 ? "COMPACT" : "PRUNE", summary(
          data.summary ?? data.reason ?? event.kind
        ), {output: data}));
        break;
      }
      case "thread.compacted":
      case "thread.forked":
      case "checkpoint.created":
      case "checkpoint.restored":
      case "checkpoint.forked":
      case "plan.delta":
      case "search.result":
      case "citation":
      case "input.required":
      case "input.resolved":
        put(record(event, "context", "CONTEXT", summary(
          data.summary ?? data.reason ?? data.prompt ?? event.kind
        ), {output: data}));
        break;
      case "approval.required":
        put(record(
          event,
          "tool",
          "APPROVAL",
          `${text(data.tool) || "Action"} · ${text(data.effect) || text(data.risk) || "Review"}`,
          {output: data}
        ));
        break;
      case "approval.resolved":
        put(record(
          event,
          "tool",
          "APPROVAL",
          `Decision · ${text(data.decision) || "resolved"}`,
          {output: data, failed: data.decision === "deny" || data.decision === "cancel"}
        ));
        break;
      case "tool.state":
      case "agent.spawned":
      case "agent.status":
      case "agent.message":
      case "agent.integration":
        put(record(
          event,
          "tool",
          "TOOL",
          eventSummary(event),
          {output: data, failed: event.kind.endsWith(".failed")}
        ));
        break;
      case "provider.attempt": {
        const sampleID = text(data.sample_id) || event.id;
        const attempt = finiteNumber(data.attempt) ?? 0;
        put(record(event, "receipt", "ATTEMPT", providerAttemptSummary(data), {
          id: `attempt-${event.turn_id}-${sampleID}-${attempt}`,
          output: data,
          failed: data.status === "failed",
          timing: Object.fromEntries(
            Object.entries({
              started_at: data.started_at,
              finished_at: data.finished_at,
              retry_at: data.retry_at,
              provider_retry_after_ms: data.provider_retry_after_ms,
              effective_delay_ms: data.effective_delay_ms,
              route_cooldown_wait_ms: data.route_cooldown_wait_ms
            }).filter(([, value]) => value !== undefined && value !== null && value !== "")
          )
        }));
        break;
      }
      case "usage": {
        const context = isRecord(data.context) ? data.context : undefined;
        if (Boolean(context?.prefix_compared)) {
          prefixes.set(
            `${event.turn_id}:${String(data.sample ?? event.sequence)}`,
            finiteNumber(context?.prefix_common_tokens) ?? 0
          );
        }
        put(record(event, "receipt", "USAGE", usageSummary(data), {
          usage: data
        }));
        break;
      }
      case "turn.completed": {
        const output = text(data.text ?? data.summary);
        if (output) {
          const id = assistantByTurn.get(event.turn_id) ?? `output-${event.turn_id}`;
          assistantByTurn.set(event.turn_id, id);
          put(record(
            event,
            "assistant",
            "ASSISTANT",
            `Output · ${lastLine(output)}`,
            {
              id,
              output,
              raw: Object.freeze({...data, text: output})
            }
          ));
        }
        put(record(
          event,
          "receipt",
          "TURN",
          eventSummary(event),
          {output: data}
        ));
        break;
      }
      case "turn.failed":
      case "turn.canceled":
      case "operation.rejected":
        put(record(
          event,
          "receipt",
          "RECEIPT",
          eventSummary(event),
          {output: data, failed: true}
        ));
        break;
      case "diagnostics.result":
        put(record(event, "verification", "VERIFY", eventSummary(event), {
          output: data,
          failed: data.status === "failed"
        }));
        break;
      case "tool.catalog.changed":
      case "mcp.health.changed":
      case "extension.control":
      case "command.execution":
      case "host.command":
      case "turn.steered":
      case "turn.reverted":
        put(record(
          event,
          "system",
          "SYSTEM",
          eventSummary(event),
          {output: data, failed: event.kind.endsWith(".failed")}
        ));
        break;
      default:
        put(record(event, "unknown", "UNKNOWN", eventSummary(event), {
          output: data,
          failed: event.kind.endsWith(".failed")
        }));
        break;
    }
  }

  const traced = trace ? traceSpans(trace, records) : [];
  const tracedRecords = new Set(traced.map((span) => span.recordID));
  const receiptTiming = new Map<string, Readonly<Record<string, unknown>>>();
  for (const event of events) {
    if (event.kind === "turn.receipt" && isRecord(event.data.latency)) {
      receiptTiming.set(event.turn_id, event.data.latency);
    }
  }
  const spans = [
    ...traced,
    ...eventSpans(events, records).filter(
      (span) => !tracedRecords.has(span.recordID)
    )
  ].map((span) => {
    const latency = receiptTiming.get(span.turnID);
    const ttftMS = span.lane === "model"
      ? finiteNumber(latency?.first_token_ms)
      : undefined;
    return ttftMS === undefined ? span : {...span, ttftMS};
  }).sort((left, right) =>
    left.startedAt - right.startedAt || left.id.localeCompare(right.id)
  );
  const prefixValues = [...prefixes.values()];
  return Object.freeze({
    records: Object.freeze(records),
    spans: Object.freeze(spans),
    prefixTokens: prefixValues.length > 0
      ? prefixValues.reduce((sum, value) => sum + value, 0) / prefixValues.length
      : undefined
  });
}

function appendTextRecord(
  event: RuntimeEvent,
  kind: "assistant",
  label: string,
  byTurn: Map<string, string>,
  records: TrajectoryRecord[],
  indexes: Map<string, number>,
  put: (record: TrajectoryRecord) => void,
  suffix = "output"
): void {
  const id = byTurn.get(event.turn_id) ?? `${suffix}-${event.turn_id}`;
  const index = indexes.get(id);
  const previous = index === undefined ? undefined : records[index];
  const output = text(previous?.output) + text(event.data.text);
  byTurn.set(event.turn_id, id);
  put(record(event, kind, label, `${suffix === "reasoning" ? "Reasoning" : "Output"} · ${
    lastLine(output)
  }`, {
    id,
    output,
    raw: Object.freeze({...event.data, text: output})
  }));
}

function appendReasoningRecord(
  event: RuntimeEvent,
  bySample: Map<string, string>,
  records: TrajectoryRecord[],
  indexes: Map<string, number>,
  put: (record: TrajectoryRecord) => void,
  completed: boolean
): void {
  const key = `${event.turn_id}:${text(event.data.sample_id) || "active"}`;
  const id = bySample.get(key) ?? `reasoning-${key}`;
  const index = indexes.get(id);
  const previous = index === undefined ? undefined : records[index];
  const output = completed
    ? text(event.data.text)
    : text(previous?.output) + text(event.data.text);
  bySample.set(key, id);
  put(record(event, "assistant", "THINK", `Reasoning · ${lastLine(output)}`, {
    id,
    output,
    raw: Object.freeze({...event.data, text: output})
  }));
}

function record(
  event: RuntimeEvent,
  kind: TrajectoryKind,
  label: string,
  value: string,
  patch: Partial<TrajectoryRecord> = {}
): TrajectoryRecord {
  return {
    id: event.id,
    sequence: event.sequence,
    createdAt: event.created_at,
    turnID: event.turn_id,
    itemID: event.item_id,
    callID: "",
    kind,
    label,
    summary: value || event.kind,
    raw: Object.freeze({...event.data}),
    searchText: searchable(event.data, event.kind, value),
    failed: false,
    ...patch
  };
}

function traceSpans(
  trace: TraceSnapshot,
  records: readonly TrajectoryRecord[]
): TrajectorySpan[] {
  const byCall = new Map(
    records.filter((value) => value.callID).map((value) => [value.callID, value.id])
  );
  const result: TrajectorySpan[] = [];
  for (const turn of trace.turns) {
    for (const span of turn.spans) {
      const startedAt = Date.parse(span.started_at);
      if (!Number.isFinite(startedAt)) continue;
      const recordID = span.call_id
        ? byCall.get(span.call_id)
        : span.kind === "tool" || span.kind === "approval"
          ? undefined
          : recordForSpan(records, turn.turn_id, span.kind);
      if (!recordID) continue;
      result.push({
        id: `${turn.turn_id}:${span.id}`,
        recordID,
        turnID: turn.turn_id,
        lane: laneForTrace(span.kind),
        kind: span.kind,
        status: span.status,
        startedAt,
        endedAt: span.ended_at ? Date.parse(span.ended_at) : undefined,
        durationMS: span.duration_ms,
        title: spanTitle(span)
      });
    }
  }
  return result;
}

function eventSpans(
  events: readonly RuntimeEvent[],
  records: readonly TrajectoryRecord[]
): TrajectorySpan[] {
  const startedByRecord = new Map<string, RuntimeEvent>();
  const endedByRecord = new Map<string, RuntimeEvent>();
  for (const event of events) {
    startedByRecord.set(event.id, event);
    const callID = text(event.data.call_id);
    if (event.kind === "tool.start" && callID) {
      startedByRecord.set(`tool-${callID}`, event);
    }
    if (event.kind === "tool.result" && callID) {
      endedByRecord.set(`tool-${callID}`, event);
    }
    if (event.kind === "output.delta") {
      const id = `output-${event.turn_id}`;
      if (!startedByRecord.has(id)) startedByRecord.set(id, event);
    }
    if (event.kind === "commentary.completed") {
      const id = commentaryNodeID(event);
      if (!startedByRecord.has(id)) {
        startedByRecord.set(id, event);
        endedByRecord.set(id, event);
      }
    }
    if (event.kind === "reasoning.delta" ||
        event.kind === "reasoning.completed") {
      const id = `reasoning-${event.turn_id}:${
        text(event.data.sample_id) || "active"
      }`;
      if (!startedByRecord.has(id)) startedByRecord.set(id, event);
      if (event.kind === "reasoning.completed") endedByRecord.set(id, event);
    }
    if (event.kind === "turn.completed") {
      const id = `output-${event.turn_id}`;
      if (startedByRecord.has(id)) {
        endedByRecord.set(id, event);
      } else {
        startedByRecord.set(id, event);
      }
    }
  }
  return records.flatMap((value) => {
    const event = startedByRecord.get(value.id);
    const ended = endedByRecord.get(value.id);
    const execution = executionWindow(value.timing);
    const startedAt = execution?.startedAt ??
      (event ? Date.parse(event.created_at) : Number.NaN);
    if (!Number.isFinite(startedAt)) return [];
    const endedAt = execution?.endedAt ??
      (ended ? Date.parse(ended.created_at) : undefined);
    const durationMS = execution?.durationMS ??
      (endedAt !== undefined && Number.isFinite(endedAt)
        ? Math.max(0, endedAt - startedAt)
        : undefined);
    return [{
      id: `event:${value.id}`,
      recordID: value.id,
      turnID: value.turnID,
      lane: laneForKind(value.kind),
      kind: value.kind,
      status: value.failed ? "error" : "ok",
      startedAt,
      endedAt,
      durationMS,
      title: value.summary
    }];
  });
}

function executionWindow(
  timing: Readonly<Record<string, unknown>> | undefined
): {startedAt: number; endedAt: number; durationMS: number} | undefined {
  const attempts = Array.isArray(timing?.attempts)
    ? timing.attempts.filter(isRecord)
    : [];
  const windows = attempts.flatMap((attempt) => {
    const startedAt = Date.parse(text(attempt.started_at));
    const endedAt = Date.parse(text(attempt.completed_at));
    if (!Number.isFinite(startedAt) || !Number.isFinite(endedAt)) return [];
    return [{startedAt, endedAt}];
  });
  if (windows.length === 0) return undefined;
  const startedAt = Math.min(...windows.map((window) => window.startedAt));
  const endedAt = Math.max(...windows.map((window) => window.endedAt));
  return {startedAt, endedAt, durationMS: Math.max(0, endedAt - startedAt)};
}

function recordForSpan(
  records: readonly TrajectoryRecord[],
  turnID: string,
  kind: TraceSpanKind
): string | undefined {
  const wanted: TrajectoryKind = kind === "model"
    ? "assistant"
    : kind === "verification"
      ? "verification"
      : kind === "tool" || kind === "approval" ? "tool" : "user";
  return records.find((record) =>
    record.turnID === turnID && record.kind === wanted
  )?.id;
}

function laneForTrace(kind: TraceSpanKind): TrajectoryLane {
  if (kind === "model") return "model";
  if (kind === "tool" || kind === "approval" || kind === "verification") {
    return "tools";
  }
  return "input";
}

function laneForKind(kind: TrajectoryKind): TrajectoryLane {
  if (kind === "assistant" || kind === "receipt") return "model";
  if (kind === "tool" || kind === "verification") return "tools";
  return "input";
}

function spanTitle(span: TraceSpan): string {
  const duration = span.duration_ms === undefined
    ? "Timing pending"
    : `${span.duration_ms} ms`;
  return `${span.kind} · ${duration}`;
}

function receiptSummary(data: Record<string, unknown>): string {
  const latency = isRecord(data.latency) ? Number(data.latency.total_ms ?? 0) : 0;
  const tokens = Number(data.input_tokens ?? 0) + Number(data.output_tokens ?? 0);
  const delegation = delegationSummary(data.delegation);
  return [
    summary(data.outcome ?? "Recorded"),
    delegation,
    latency > 0 ? `${latency} ms` : "",
    tokens > 0 ? `${tokens} tokens` : ""
  ].filter(Boolean).join(" · ");
}

function delegationSummary(value: unknown): string {
  if (!isRecord(value)) return "";
  switch (text(value.outcome)) {
    case "delegated": {
      const spawned = finiteNumber(value.spawned) ?? 0;
      return `Delegated to ${spawned} subagent${spawned === 1 ? "" : "s"}`;
    }
    case "retained_parent":
      return "Parent execution";
    case "blocked":
      return "Delegation blocked";
    default:
      return "";
  }
}

function providerAttemptSummary(data: Record<string, unknown>): string {
  const status = text(data.status);
  const failure = text(data.failure_code);
  const stop = text(data.stop_reason);
  if (status === "retry_wait" && failure === "rate_limit") {
    return "Provider rate limited, waiting to retry";
  }
  if (status === "retry_wait") {
    return `Provider retrying · ${failure || "request failed"}`;
  }
  if (status === "incomplete") {
    return `Provider output incomplete · ${stop || "continuing"}`;
  }
  if (status === "completed" && stop === "tool_use") {
    return "Tool call complete, continuing this turn";
  }
  if (status === "completed") {
    return `Provider completed · ${stop || "end_turn"}`;
  }
  if (status === "failed") {
    return `Provider attempt failed · ${failure || "error"}`;
  }
  return `Provider attempt ${status || "started"}`;
}

function usageSummary(data: Record<string, unknown>): string {
  const total = Number(data.input_tokens ?? 0) +
    Number(data.output_tokens ?? 0);
  return `${text(data.provider) || "Model"} · ${total} tokens`;
}

function eventSummary(event: RuntimeEvent): string {
  return summary(
    event.data.summary ??
    event.data.message ??
    event.data.status ??
    event.data.state ??
    event.kind
  );
}

function argumentSummary(value: unknown): string {
  if (!isRecord(value)) return firstLine(value) || "Waiting";
  for (const key of ["description", "path", "file_path", "query", "pattern", "cmd", "command"]) {
    if (value[key] !== undefined) return firstLine(value[key]);
  }
  return firstLine(JSON.stringify(value));
}

function searchable(...values: unknown[]): string {
  return values.map((value) => {
    try {
      return typeof value === "string" ? value : JSON.stringify(value);
    } catch {
      return String(value);
    }
  }).join(" ").toLocaleLowerCase();
}

function summary(value: unknown): string {
  const result = firstLine(value);
  return result.length > 180 ? `${result.slice(0, 177)}...` : result;
}

function firstLine(value: unknown): string {
  return text(value).split(/\r?\n/).map((line) => line.trim()).find(Boolean) ?? "";
}

function lastLine(value: string): string {
  const lines = value.split(/\r?\n/);
  for (let index = lines.length - 1; index >= 0; index -= 1) {
    const current = lines[index]?.trim();
    if (current) return summary(current);
  }
  return "";
}

function text(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function finiteNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : undefined;
}
