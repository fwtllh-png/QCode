import type {RuntimeEvent} from "../protocol";

export interface ContextAttribution {
  inputTokens: number;
  capacity?: number;
  outputReserve?: number;
  hardInputTokens?: number;
  observed: boolean;
  estimatedTokens?: number;
  stableTokens?: number;
  toolTokens?: number;
  messageTokens?: number;
  framingTokens?: number;
}

// Quantities in a context meter must describe one frozen model request.
// A newly selected model's capacity cannot be used to divide an older input.
export function contextFromSample(value: unknown): ContextAttribution | undefined {
  const sample = record(value);
  if (!sample) return undefined;
  const measured = number(sample.measured_input_tokens);
  const estimated = number(sample.estimated_tokens);
  const input = measured ?? number(sample.window_full_active_tokens) ?? estimated;
  if (input === undefined) return undefined;
  const reserve = number(sample.window_output_reserve);
  const hard = number(sample.window_hard_input_tokens);
  const capacity = number(sample.window_context_tokens) ?? (
    hard !== undefined && reserve !== undefined ? hard + reserve : undefined
  );
  return {
    inputTokens: input, capacity, outputReserve: reserve, hardInputTokens: hard,
    observed: measured !== undefined, estimatedTokens: estimated,
    ...(estimated === undefined ? {} : {
      stableTokens: sum(sample.stable_tokens, sample.dynamic_tokens, sample.continuation_tokens),
      toolTokens: sum(sample.tool_definition_tokens, sample.history_tool_tokens),
      messageTokens: sum(sample.history_user_tokens, sample.history_assistant_tokens, sample.history_other_tokens),
      framingTokens: number(sample.provider_framing_tokens) ?? 0
    })
  };
}

export function contextFromReceipt(receipt?: Readonly<Record<string, unknown>>): ContextAttribution | undefined {
  const sample = contextFromSample(receipt?.context_sample);
  if (sample) return sample;
  const budget = record(receipt?.context_budget);
  if (budget?.measurement_source !== "provider_usage" && budget?.measurement_source !== "request_estimate") return undefined;
  const input = number(budget.active_tokens);
  if (input === undefined) return undefined;
  return {
    inputTokens: input, capacity: number(budget.max_context_tokens),
    outputReserve: number(budget.output_reserve), hardInputTokens: number(budget.hard_input_tokens),
    observed: budget.measurement_source === "provider_usage"
  };
}

export function latestContextAttribution(events: readonly RuntimeEvent[]): ContextAttribution | undefined {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (event.kind === "usage" || event.kind === "provider.attempt") {
      const context = contextFromSample(event.data.context);
      if (context) return context;
    }
    if (["turn.completed", "turn.failed", "turn.canceled"].includes(event.kind)) {
      const receipt = record(event.data.receipt);
      if (receipt) return contextFromReceipt(receipt);
    }
  }
  return undefined;
}

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}
function number(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : undefined;
}
function sum(...values: unknown[]): number {
  return values.reduce<number>((total, value) => total + (number(value) ?? 0), 0);
}
