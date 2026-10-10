import {describe, expect, it} from "vitest";
import type {RuntimeEvent} from "../protocol";
import {contextFromReceipt, contextFromSample, latestContextAttribution} from "./contextUsage";

function event(kind: RuntimeEvent["kind"], data: Record<string, unknown>): RuntimeEvent {
  return {kind, data} as RuntimeEvent;
}

describe("context usage source", () => {
  it("keeps zero distinct from unknown and uses a frozen request denominator", () => {
    expect(contextFromSample({})).toBeUndefined();
    expect(contextFromSample({estimated_tokens: -1})).toBeUndefined();
    expect(contextFromSample({estimated_tokens: 50, measured_input_tokens: 0,
      window_context_tokens: 100, window_output_reserve: 20, window_hard_input_tokens: 80}))
      .toMatchObject({inputTokens: 0, observed: true, capacity: 100, outputReserve: 20, hardInputTokens: 80});
    expect(contextFromSample({estimated_tokens: 0, window_hard_input_tokens: 80, window_output_reserve: 20}))
      .toMatchObject({inputTokens: 0, observed: false, capacity: 100});
    expect(contextFromSample({estimated_tokens: 10})?.capacity).toBeUndefined();
  });

  it("uses the latest request when a later attempt fails without usage", () => {
    const events = [
      event("usage", {context: {estimated_tokens: 60, measured_input_tokens: 70, window_context_tokens: 100}}),
      event("provider.attempt", {context: {estimated_tokens: 40, window_full_active_tokens: 45, window_context_tokens: 200}})
    ];
    expect(latestContextAttribution(events)).toMatchObject({inputTokens: 45, capacity: 200, observed: false});
    events.push(event("turn.failed", {receipt: {context_sample: {
      estimated_tokens: 40, window_full_active_tokens: 45, window_context_tokens: 200
    }}}));
    expect(latestContextAttribution(events)).toEqual(latestContextAttribution(events.slice(-1)));
  });

  it("does not fall back to older request usage when the last receipt has no sample", () => {
    expect(latestContextAttribution([
      event("usage", {context: {estimated_tokens: 60}}),
      event("turn.canceled", {receipt: {context_budget: {active_tokens: 9999}}})
    ])).toBeUndefined();
    expect(contextFromReceipt({context_budget: {active_tokens: 9999}})).toBeUndefined();
    expect(contextFromReceipt({context_budget: {measurement_source: "request_estimate",
      active_tokens: 50, max_context_tokens: 100, output_reserve: 20, hard_input_tokens: 80}}))
      .toMatchObject({inputTokens: 50, capacity: 100, outputReserve: 20, hardInputTokens: 80, observed: false});
  });
});
