import {describe, expect, it} from "vitest";
import type {RuntimeEvent, TraceSnapshot} from "../protocol";
import {projectTrajectory} from "./trajectory";

describe("projectTrajectory", () => {
  it("coalesces streaming records and links trace spans to tool calls", () => {
    const events = [
      event(1, "turn.started", {display_prompt: "Read README"}),
      event(2, "reasoning.delta", {text: "Find "}),
      event(3, "reasoning.delta", {text: "the file"}),
      event(4, "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(5, "tool.result", {
        call_id: "call-1",
        tool: "file_read",
        output: "# Project",
        is_error: false
      }),
      event(6, "output.delta", {text: "Summary"})
    ];
    const trace: TraceSnapshot = {
      version: 1,
      session_id: "session",
      through_sequence: 6,
      turns: [{
        turn_id: "turn",
        status: "ok",
        spans: [{
          id: 2,
          parent_id: 1,
          kind: "tool",
          status: "ok",
          started_at: "2026-01-01T00:00:04Z",
          ended_at: "2026-01-01T00:00:05Z",
          duration_ms: 1_000,
          call_id: "call-1"
        }]
      }]
    };

    const projection = projectTrajectory(events, trace);

    expect(projection.records.map((record) => record.label)).toEqual([
      "USER", "THINK", "TOOL", "ASSISTANT"
    ]);
    expect(projection.records.find((record) => record.callID === "call-1"))
      .toMatchObject({summary: "file_read · README.md -> # Project"});
    expect(projection.spans.find((span) => span.recordID === "tool-call-1"))
      .toMatchObject({
      recordID: "tool-call-1",
      lane: "tools",
      durationMS: 1_000
      });
  });

  it("keeps event rows usable when trace timing is unavailable", () => {
    const projection = projectTrajectory([
      event(1, "turn.started", {display_prompt: "Hello"}),
      event(2, "turn.completed", {text: "Hi"})
    ]);

    expect(projection.records.map((record) => record.label)).toEqual([
      "USER", "ASSISTANT", "TURN"
    ]);
    expect(projection.records[1]).toMatchObject({output: "Hi"});
    expect(projection.spans).toHaveLength(3);
    expect(projection.spans.every((span) => span.durationMS === undefined)).toBe(true);
  });

  it("distinguishes pruning from history replacement and hides lifecycle duplicates", () => {
    const projection = projectTrajectory([
      event(1, "turn.compaction", {
        phase: "mid_turn",
        summary: "pruned tool results",
        pruned_tool_results: 2,
        pruned_bytes: 4096
      }),
      event(2, "turn.compaction", {
        compaction_id: "compact-1",
        phase: "mid_turn",
        summary: "replaced history",
        removed_messages: 8,
        original_bytes: 32000,
        retained_bytes: 12000
      }),
      event(3, "turn.compaction", {
        compaction_id: "compact-1",
        phase: "post_turn",
        status: "completed",
        summary: "compaction completed"
      })
    ]);

    expect(projection.records.map((record) => record.label)).toEqual([
      "PRUNE", "COMPACT"
    ]);
    expect(projection.records.map((record) => record.summary)).toEqual([
      "pruned tool results", "replaced history"
    ]);
  });

  it("uses event pairs when trace only exposes aggregate tool timing", () => {
    const events = [
      event(1, "turn.started", {display_prompt: "Inspect files"}),
      event(2, "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(3, "tool.result", {
        call_id: "call-1",
        tool: "file_read",
        output: "# Project",
        is_error: false
      }),
      event(4, "turn.completed", {text: "Done"})
    ];
    const trace: TraceSnapshot = {
      version: 1,
      session_id: "session",
      through_sequence: 4,
      turns: [{
        turn_id: "turn",
        status: "ok",
        spans: [{
          id: 2,
          parent_id: 1,
          kind: "tool",
          status: "ok",
          started_at: "2026-01-01T00:00:02Z",
          ended_at: "2026-01-01T00:00:03Z",
          duration_ms: 1_000
        }, {
          id: 3,
          parent_id: 1,
          kind: "verification",
          status: "ok",
          started_at: "2026-01-01T00:00:04Z",
          ended_at: "2026-01-01T00:00:04Z",
          duration_ms: 0
        }]
      }]
    };

    const projection = projectTrajectory(events, trace);
    const toolSpans = projection.spans.filter((span) => span.lane === "tools");

    expect(toolSpans).toHaveLength(1);
    expect(toolSpans[0]).toMatchObject({
      recordID: "tool-call-1",
      durationMS: 1_000
    });
    expect(projection.spans.some((span) => span.kind === "verification")).toBe(false);
    expect(projection.records.some(
      (record) => record.kind === "assistant" && record.output === "Done"
    )).toBe(true);
  });

  it("exposes approval semantics and model TTFT in the inspector projection", () => {
    const projection = projectTrajectory([
      event(1, "turn.started", {display_prompt: "Edit a file"}),
      event(2, "output.delta", {text: "Preparing"}),
      event(3, "approval.required", {
        request_id: "approval",
        call_id: "write",
        tool: "file_write",
        effect: "workspace_write"
      }),
      event(4, "usage", {
        provider: "fixture",
        input_tokens: 40,
        output_tokens: 8,
        reasoning_tokens: 3
      }),
      event(5, "turn.receipt", {
        outcome: "changed",
        latency: {first_token_ms: 250, provider_ms: 900}
      }),
      event(6, "turn.completed", {text: "Done"})
    ]);

    expect(projection.records.find((record) => record.label === "APPROVAL"))
      .toMatchObject({summary: "file_write · workspace_write"});
    expect(projection.records.find((record) => record.label === "USAGE"))
      .toMatchObject({summary: "fixture · 48 tokens"});
    expect(projection.spans.find((span) => span.recordID === "output-turn"))
      .toMatchObject({ttftMS: 250});
  });

  it("summarizes observed adaptive delegation outcomes", () => {
    const delegated = projectTrajectory([
      event(1, "turn.receipt", {
        outcome: "changed",
        delegation: {
          mode: "adaptive",
          outcome: "delegated",
          attempts: 2,
          spawned: 2
        }
      })
    ]);
    expect(delegated.records[0]?.summary)
      .toBe("changed · Delegated to 2 subagents");

    const retained = projectTrajectory([
      event(1, "turn.receipt", {
        outcome: "answered",
        delegation: {
          mode: "adaptive",
          outcome: "retained_parent",
          attempts: 0,
          spawned: 0
        }
      })
    ]);
    expect(retained.records[0]?.summary)
      .toBe("answered · Parent execution");
  });

  it("aggregates average common prefix length", () => {
    const projection = projectTrajectory([
      event(1, "turn.started", {display_prompt: "Cache audit"}),
      event(2, "usage", {
        sample: 1,
        provider: "fixture",
        input_tokens: 100,
        cached_tokens: 70,
        context: {prefix_compared: true, prefix_common_tokens: 60}
      }),
      event(3, "usage", {
        sample: 2,
        provider: "fixture",
        input_tokens: 200,
        cached_tokens: 150,
        context: {prefix_compared: true, prefix_common_tokens: 120}
      }),
      event(4, "usage", {
        sample: 2,
        provider: "fixture",
        context: {prefix_compared: true, prefix_common_tokens: 180}
      }),
      event(5, "turn.receipt", {
        outcome: "changed",
        latency: {first_token_ms: 300, provider_ms: 900}
      }),
      event(6, "turn.completed", {text: "Done"})
    ]);

    expect(projection.prefixTokens).toBe(120);
  });

  it("includes compared zero-length prefixes without inventing cache samples", () => {
    const projection = projectTrajectory([
      event(1, "turn.started", {display_prompt: "Plain turn"}),
      event(2, "usage", {
        provider: "fixture",
        input_tokens: 0,
        context: {prefix_compared: true, prefix_common_tokens: 0}
      }),
      event(3, "turn.completed", {text: "Hi"})
    ]);

    expect(projection.prefixTokens).toBe(0);
  });

  it("projects provider attempts as structured transport facts", () => {
    const projection = projectTrajectory([
      event(1, "turn.started", {display_prompt: "Work"}),
      event(2, "provider.attempt", {
        sample_id: "sample-1",
        attempt: 1,
        status: "retry_wait",
        failure_code: "rate_limit",
        http_status: 429,
        provider_retry_after_ms: 2500
      }),
      event(3, "provider.attempt", {
        sample_id: "sample-1",
        attempt: 1,
        status: "completed",
        stop_reason: "tool_use"
      })
    ]);
    const attempts = projection.records.filter((record) => record.label === "ATTEMPT");
    expect(attempts).toHaveLength(1);
    expect(attempts[0]).toMatchObject({
      summary: "Tool call complete, continuing this turn",
      failed: false
    });
  });
});

function event(
  sequence: number,
  kind: string,
  data: Record<string, unknown>
): RuntimeEvent {
  return {
    version: 1,
    id: `event-${sequence}`,
    kind,
    operation_id: "operation",
    thread_id: "thread",
    turn_id: "turn",
    item_id: `item-${sequence}`,
    sequence,
    created_at: `2026-01-01T00:00:0${sequence}Z`,
    data
  };
}

describe("trajectory projection skips transient draft streams", () => {
  it("renders no unknown rows for output.draft and output.discarded", () => {
    const {records} = projectTrajectory([
      event(1, "turn.started", {prompt: "build"}),
      event(2, "output.draft", {text: "partial answer"}),
      event(3, "output.draft", {text: "partial answer continued"}),
      event(4, "output.discarded", {sample_id: "s1"}),
      event(5, "output.delta", {text: "final answer"}),
      event(6, "turn.completed", {text: "final answer"})
    ]);
    expect(records.length).toBeGreaterThan(0);
    expect(records.filter((entry) => entry.kind === "unknown")).toEqual([]);
    expect(records.filter((entry) =>
      entry.summary.includes("partial answer"))).toEqual([]);
    expect(records.some((entry) => entry.kind === "assistant" &&
      entry.summary.includes("final answer"))).toBe(true);
  });

  it("files queue and title lifecycle events as system rows", () => {
    const {records} = projectTrajectory([
      event(1, "turn.queued", {turn_id: "turn"}),
      event(2, "turn.queue.updated", {position: 1}),
      event(3, "turn.queue.removed", {}),
      event(4, "turn.withdrawn", {reason: "user"}),
      event(5, "session.title.updated", {title: "S6"})
    ]);
    const system = records.filter((entry) => entry.kind === "system");
    expect(system).toHaveLength(5);
    expect(records.filter((entry) => entry.kind === "unknown")).toEqual([]);
  });
});
