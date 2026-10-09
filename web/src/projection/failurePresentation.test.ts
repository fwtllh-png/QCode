import {describe, expect, it} from "vitest";
import type {RuntimeEvent} from "../protocol";
import {ConversationProjection, projectConversation} from "./conversation";

function event(sequence: number, kind: string, data: Record<string, unknown>, turnID = "turn"): RuntimeEvent {
  return {version: 1, id: `event-${sequence}`, kind, operation_id: "operation", thread_id: "thread",
    turn_id: turnID, item_id: `item-${sequence}`, sequence, created_at: "2026-01-01T00:00:00Z", data};
}

function last(events: RuntimeEvent[]) {
  const view = projectConversation(events);
  return view.nodes.get(view.order.at(-1)!);
}

describe("failure recovery presentation", () => {
  it.each([
    ["token_budget_exhausted", "Token limit reached"],
    ["cost_budget_exhausted", "Cost limit reached"],
    ["provider_quota_exhausted", "Provider quota exhausted"],
    ["provider_rate_limited", "Provider rate limit reached"],
    ["provider_throughput", "Provider throughput limit reached"],
    ["provider_retry_exhausted", "Model request retries exhausted"],
    ["provider_tool_argument_repair_exhausted", "Tool argument repair exhausted"],
    ["provider_auth", "Provider authentication required"],
    ["provider_invalid_request", "Model configuration needs attention"],
    ["provider_response_invalid", "Model response rejected"],
    ["provider_unsupported_content", "Provider rejected the content"],
    ["context_window_exceeded", "Model context limit reached"],
    ["verification_unavailable", "Verification unavailable"],
    ["verification_failed", "Verification not passed"]
  ])("uses structured reason %s", (reason, title) => {
    expect(last([event(1, "turn.failed", {message: "recorded evidence",
      fault: {reason, disposition: "resume_turn", recovery_action: "specific runtime action"}})]))
      .toMatchObject({title, text: "recorded evidence", nextStep: "specific runtime action", blocked: true});
  });

  it.each(["provider rate limit retry budget exhausted", "permission denied", "token quota exhausted"])(
    "does not infer a cause from %s", (message) => {
      expect(last([event(1, "turn.failed", {message, fault: {origin: "runtime", disposition: "resume_turn"}})]))
        .toMatchObject({title: "Recovery required", text: message});
    }
  );

  it("keeps storage failures distinct from stale convergence and provider failures", () => {
    const node = last([
      event(1, "provider.attempt", {sample_id: "sample", attempt: 1, status: "failed", failure_code: "rate_limit"}),
      event(2, "turn.failed", {message: "storage unavailable", fault: {origin: "persistence", disposition: "resume_turn"},
        convergence: {cause: "declared_incomplete", summary: "unrelated partial summary"}})
    ]);
    expect(node).toMatchObject({title: "Storage recovery required", text: "storage unavailable"});
    expect(node?.kind === "status" && node.nextStep).not.toMatch(/Continue from the last durable step/);
  });

  it("does not replace storage recovery with an earlier permission denial", () => {
    expect(last([
      event(1, "turn.started", {posture: "never"}),
      event(2, "tool.start", {call_id: "write", tool: "file_apply"}),
      event(3, "tool.result", {call_id: "write", tool: "file_apply", is_error: true,
        recovery: {error_category: "permission_denied"}}),
      event(4, "turn.failed", {fault: {origin: "persistence", disposition: "resume_turn"},
        convergence: {cause: "declared_incomplete"}})
    ])).toMatchObject({title: "Storage recovery required", recovery: {action: ""}});
  });

  it("shows pending actions and actual recorded recovery without counting duplicate events", () => {
    const records = [
      event(1, "provider.attempt", {sample_id: "sample", attempt: 1, status: "retry_wait", failure_code: "rate_limit"}),
      event(2, "provider.attempt", {sample_id: "sample", attempt: 2, status: "retry_wait", failure_code: "server"}),
      event(3, "provider.attempt", {sample_id: "sample", attempt: 3, status: "started", reason: "tool_argument_repair"}),
      event(4, "provider.attempt", {sample_id: "other", attempt: 1, status: "retry_wait", failure_code: "rate_limit"}, "other"),
      event(5, "turn.failed", {convergence: {cause: "declared_incomplete", summary: "work remains",
        pending_actions: ["Install the missing dependency.", "Run the checks."]}})
    ];
    const node = last(records);
    expect(node).toMatchObject({title: "Task incomplete", text: "work remains",
      nextStep: "Install the missing dependency.\nRun the checks.",
      attemptedRecovery: "Recorded recovery: 1 rate-limit waits scheduled; 1 model request retries scheduled; 1 tool argument regenerations started."});
    const incremental = new ConversationProjection();
    incremental.applyAll(records.slice(0, 3));
    incremental.applyAll(records);
    expect(incremental.snapshot().nodes.get(node!.id)).toEqual(node);
  });

  it("does not invent prior retries, and clears recovery guidance on withdrawal", () => {
    const failed = event(1, "turn.failed", {message: "failure", fault: {reason: "provider_retry_exhausted", disposition: "resume_turn"}});
    expect(last([failed])).toMatchObject({title: "Model request retries exhausted", attemptedRecovery: undefined});
    expect(last([failed, event(2, "turn.withdrawn", {})])).toMatchObject({title: "Withdrawn", nextStep: undefined, attemptedRecovery: undefined, recoverable: false});
  });
});
