import type {RuntimeEvent} from "../protocol";

export interface FailurePresentation {
  title: string;
  text: string;
  blocked?: boolean;
  warning?: boolean;
  nextStep?: string;
}

// Classification uses Runtime facts. Free-form messages remain evidence and
// must not turn an unknown fault into a permission, quota or retry failure.
export function failurePresentation(event: RuntimeEvent): FailurePresentation {
  const data = event.data;
  const fault = record(data.fault);
  const convergence = record(data.convergence);
  const reason = text(fault.reason);
  const origin = text(fault.origin);
  const cause = text(convergence.cause);
  const fallback = text(data.outcome ?? data.message ?? data.reason) || "Turn did not complete";
  const recoverable = ["retry_step", "retry_turn", "resume_turn"].includes(text(fault.disposition));
  const result: FailurePresentation = {
    title: event.kind === "turn.canceled" ? "Canceled" : "Failed",
    text: fallback,
    blocked: recoverable || Boolean(cause),
    warning: recoverable || Boolean(cause)
  };
  if (event.kind === "turn.canceled") {
    if (data.reason === "user_interrupted") {
      return {title: "Paused", text: "Paused by user.", warning: true};
    }
    return result;
  }

  const classified: Record<string, [string, string]> = {
    token_budget_exhausted: ["Token limit reached", "Increase the run's token budget before continuing."],
    cost_budget_exhausted: ["Cost limit reached", "Increase the run's cost budget before continuing."],
    provider_quota_exhausted: ["Provider quota exhausted", "Restore the provider balance or wait for its quota to reset before continuing."],
    provider_rate_limited: ["Provider rate limit reached", "Wait for the provider cooldown before continuing."],
    provider_throughput: ["Provider throughput limit reached", "Wait for capacity, reduce the request, or adjust the configured throughput limit."],
    provider_retry_exhausted: ["Model request retries exhausted", "Check provider availability or select another model before retrying."],
    provider_tool_argument_repair_exhausted: ["Tool argument repair exhausted", "Check the provider or select another model before continuing. Rejected calls were not executed."],
    provider_auth: ["Provider authentication required", "Check the API key and access permissions in Connection settings before retrying."],
    provider_invalid_request: ["Model configuration needs attention", "Correct the model or request configuration in Connection settings before retrying."],
    provider_unsupported_content: ["Provider rejected the content", "Review the provider's supported content and input restrictions before retrying."],
    provider_response_invalid: ["Model response rejected", "Inspect the provider response failure or select another model before retrying."],
    context_window_exceeded: ["Model context limit reached", "Reduce required context or select a model with a larger context window."],
    permission_denied: ["Permission change required", "Review the permission requirement before continuing."],
    verification_unavailable: ["Verification unavailable", "Restore the verification environment or supply the required execution evidence before continuing."],
    verification_failed: ["Verification not passed", "Inspect the failed checks and resolve their cause before continuing."]
  };
  const known = Object.hasOwn(classified, reason) ? classified[reason] : undefined;
  if (known) {
    result.title = known[0];
    result.nextStep = text(fault.recovery_action) || known[1];
  } else if (["persistence", "kernel", "projection"].includes(origin)) {
    result.title = origin === "persistence" ? "Storage recovery required" : "Runtime recovery required";
    result.nextStep = text(fault.recovery_action) ||
      "Inspect the recorded failure and restore runtime health before attempting recovery.";
  } else if (origin === "verification" ||
    (cause === "repair_budget" && convergence.repair_kind === "verification")) {
    result.title = "Verification not passed";
    result.nextStep = text(fault.recovery_action) || "Inspect the check results, resolve the failure, then continue.";
  } else if (cause) {
    const titles: Record<string, string> = {
      declared_incomplete: "Task incomplete",
      no_progress: "Progress stalled",
      repair_budget: "Repair attempts exhausted",
      step_limit: "Step limit reached",
      output_limit: "Output limit reached"
    };
    result.title = Object.hasOwn(titles, cause) ? titles[cause]! : "Task incomplete";
    result.text = text(convergence.summary) || fallback;
    result.nextStep = text(fault.recovery_action) ||
      (Array.isArray(convergence.pending_actions)
        ? convergence.pending_actions.filter((item): item is string => typeof item === "string").join("\n")
        : "") || "Review the unfinished work and remaining conditions before continuing.";
  } else if (origin === "provider") {
    result.title = "Model request failed";
    result.nextStep = text(fault.recovery_action) || "Inspect the provider failure before retrying.";
  } else if (recoverable) {
    result.title = "Recovery required";
    result.nextStep = text(fault.recovery_action) || "Review the recorded failure before attempting recovery.";
  }
  // Preserve the existing readable numeric rendering only for the exact token
  // budget format. It is not used to classify other free-form failures.
  const budget = fallback.match(/^token budget exhausted: projected (\d+), limit (\d+)$/);
  if ((reason === "token_budget_exhausted" || !reason && !origin && !cause) && budget) {
    result.title = "Token limit reached";
    result.text = `The next model call would exceed this run's token limit (${integer(budget[1]!)} projected, ${integer(budget[2]!)} allowed).`;
    result.nextStep ||= "Increase the run's token budget before continuing.";
  }
  result.nextStep ||= text(fault.recovery_action) || undefined;
  return result;
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : {};
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function integer(value: string): string {
  return value.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}
