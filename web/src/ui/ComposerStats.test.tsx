import {cleanup, fireEvent, render, screen, within} from "@testing-library/react";
import {afterEach, describe, expect, it} from "vitest";
import {ComposerStats} from "./ComposerStats";

afterEach(cleanup);

const usage = {turns: 26, calls: 202, total_tokens: 12_262_039, cost_microunits: 0, cost_known: false};

function openLatest() {
  fireEvent.click(screen.getByRole("button", {name: /Run statistics/}));
  return within(screen.getByRole("region", {name: "Latest turn details"}));
}

describe("ComposerStats", () => {
  it("never replaces zero or missing turn usage with session usage", () => {
    const {rerender} = render(<ComposerStats receipt={{input_tokens: 0, output_tokens: 0}}
      usage={usage} running={false} previous={false} />);
    expect(openLatest().getAllByText("0", {selector: "dd"})).toHaveLength(3);
    fireEvent.keyDown(document, {key: "Escape"});
    rerender(<ComposerStats receipt={{input_tokens: 0}}
      usage={usage} running={false} previous={false} />);
    const latest = openLatest();
    expect(latest.getAllByText("—", {selector: "dd"}).length).toBeGreaterThan(0);
    expect(latest.getByText("Not reported")).toBeTruthy();
    expect(latest.queryByText("0%")).toBeNull();
    expect(latest.queryByText("12,262,039")).toBeNull();
    expect(latest.getByText("Unknown")).toBeTruthy();
  });

  it("does not infer measurements from an unmeasured receipt", () => {
    render(<ComposerStats receipt={{measurement_recorded: false, input_tokens: 0,
      output_tokens: 0, tool_execution: null}} running={false} previous={false} />);
    const latest = openLatest();
    expect(latest.queryByText("0", {selector: "dd"})).toBeNull();
  });

  it("uses 1024 per K and keeps reasoning as a subset of output", () => {
    render(<ComposerStats receipt={{
      input_tokens: 24_600, output_tokens: 2_855, reasoning_tokens: 2_434,
      tool_execution: null, latency: {total_ms: 15107, first_token_ms: 1495, tool_ms: 0},
      cost_known: true, cost_microunits: 1
    }} running={false} previous={false} />);
    expect(screen.getByRole("button").textContent).toBe("");
    const latest = openLatest();
    expect(latest.getByText("27,455")).toBeTruthy();
    expect(latest.getByText("2,434")).toBeTruthy();
    expect(latest.getByText("0 ms")).toBeTruthy();
    expect(latest.getByText("$0.000001")).toBeTruthy();
    expect(latest.getByText("0", {selector: "dd"})).toBeTruthy();
  });

  it("labels previous receipts during a run and closes by Escape or outside click", () => {
    render(<ComposerStats receipt={{input_tokens: 1, output_tokens: 2}}
      running previous terminal="turn.canceled" />);
    const trigger = screen.getByRole("button", {name: /Run statistics/});
    fireEvent.click(trigger);
    expect(screen.getByText("Previous turn")).toBeTruthy();
    expect(screen.getByText(/Running ·/)).toBeTruthy();
    expect(screen.getByText("Canceled")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
    fireEvent.keyDown(document, {key: "Escape"});
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(trigger);
    fireEvent.click(trigger);
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps session billing turns distinct from unknown lifecycle counts", () => {
    render(<ComposerStats usage={{...usage, priced_calls: 1, unpriced_calls: 2,
      cost_microunits: 12_300}} running={false} previous={false} />);
    fireEvent.click(screen.getByRole("button", {name: /Run statistics/}));
    const session = within(screen.getByRole("region", {name: "Session totals"}));
    expect(session.queryByText("26", {selector: "dd"})).toBeNull();
    expect(session.getByText("202", {selector: "dd"})).toBeTruthy();
    expect(session.getByText("$0.0123+ (partly unpriced)")).toBeTruthy();
  });

  it("uses current context for the ring, preserving zero and unknown samples", () => {
    const receipt = {input_tokens: 900_000, output_tokens: 100_000,
      context_budget: {measurement_source: "provider_usage", active_tokens: 32_768, max_context_tokens: 131_072}};
    const {rerender} = render(<ComposerStats receipt={receipt} usage={usage}
      running={false} previous={false} />);
    const trigger = screen.getByRole("button", {name: /25% of context used/});
    expect(trigger.querySelector(".contextFill")?.getAttribute("stroke-dasharray")).toBe("25 100");
    fireEvent.click(trigger);
    expect(screen.getByText("32K / 128K tokens")).toBeTruthy();
    rerender(<ComposerStats receipt={receipt} attribution={{inputTokens: 0, observed: false, capacity: 131_072, estimatedTokens: 0,
      stableTokens: 0, toolTokens: 0, messageTokens: 0, framingTokens: 0}}
      running={false} previous={false} />);
    expect(screen.getByRole("button", {name: /0% of context used/})).toBeTruthy();
    rerender(<ComposerStats capacity={131_072} running={false} previous={false} />);
    expect(screen.getByRole("button", {name: /Context usage unavailable/})).toBeTruthy();
    expect(screen.getByText("— / 128K tokens")).toBeTruthy();
  });

  it("uses measured input and frozen capacity after changing models or replaying a receipt", () => {
    const receipt = {context_sample: {estimated_tokens: 512, measured_input_tokens: 1024,
      window_full_active_tokens: 1024, window_context_tokens: 4096,
      window_output_reserve: 1024, window_hard_input_tokens: 3072,
      stable_tokens: 128, history_user_tokens: 384},
      context_budget: {active_tokens: 999999, max_context_tokens: 4096}};
    const {container, rerender} = render(<ComposerStats receipt={receipt} capacity={8192}
      running={false} previous={false} />);
    fireEvent.click(screen.getByRole("button", {name: /25% of context used/}));
    expect(screen.getByText("1K / 4K tokens")).toBeTruthy();
    const context = within(screen.getByRole("region", {name: "Context usage"}));
    expect(context.getByText("3,072")).toBeTruthy();
    expect(context.getByText("1,024")).toBeTruthy();
    expect(context.getByText("2,048")).toBeTruthy();
    const bar = container.querySelector(".contextBar")!;
    expect(bar.querySelector<HTMLElement>('[data-tone="stable"]')?.style.width).toBe("6.25%");
    expect(bar.querySelector<HTMLElement>('[data-tone="messages"]')?.style.width).toBe("18.75%");
    expect(bar.querySelector<HTMLElement>('[data-tone="reserved"]')?.style.width).toBe("25%");
    rerender(<ComposerStats receipt={receipt} capacity={2048} running={false} previous={false} />);
    expect(screen.getByRole("button", {name: /25% of context used/})).toBeTruthy();
  });

  it("does not present old unprojected history budgets as request usage", () => {
    render(<ComposerStats capacity={1_000_000} receipt={{context_budget: {
      active_tokens: 1_133_367, max_context_tokens: 1_000_000
    }}} running={false} previous={false} />);
    fireEvent.click(screen.getByRole("button", {name: /Context usage unavailable/}));
    expect(screen.queryByText("113%")).toBeNull();
    expect(screen.getByText("Context usage has not been recorded yet.")).toBeTruthy();
  });
});
