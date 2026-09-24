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
      context_budget: {active_tokens: 32_768, max_context_tokens: 131_072}};
    const {rerender} = render(<ComposerStats receipt={receipt} usage={usage}
      running={false} previous={false} />);
    const trigger = screen.getByRole("button", {name: /25% of context used/});
    expect(trigger.querySelector(".contextFill")?.getAttribute("stroke-dasharray")).toBe("25 100");
    fireEvent.click(trigger);
    expect(screen.getByText("32K / 128K tokens")).toBeTruthy();
    rerender(<ComposerStats receipt={receipt} attribution={{estimatedTokens: 0,
      stableTokens: 0, toolTokens: 0, messageTokens: 0, framingTokens: 0}}
      running={false} previous={false} />);
    expect(screen.getByRole("button", {name: /0% of context used/})).toBeTruthy();
    rerender(<ComposerStats capacity={131_072} running={false} previous={false} />);
    expect(screen.getByRole("button", {name: /Context usage unavailable/})).toBeTruthy();
    expect(screen.getByText("— / 128K tokens")).toBeTruthy();
  });
});
