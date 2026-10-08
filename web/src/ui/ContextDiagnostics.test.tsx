import {cleanup, fireEvent, render, screen} from "@testing-library/react";
import {afterEach, describe, expect, it} from "vitest";
import {ContextDiagnostics} from "./ContextDiagnostics";

afterEach(cleanup);
describe("ContextDiagnostics", () => {
  it("keeps identities hidden until details are opened and preserves measured zero", () => {
    render(<ContextDiagnostics receipt={{context_projection: {digest: "source-only-in-details", recovery_only: false,
      input_tokens: 200, output_reserve: 100, raw_tokens: 0, raw_token_limited: true, raw_token_limit: 0,
      omissions: [{reason: "history_token_ceiling"}]}, context_recovery: {calls: 0, failed: 0, bytes: 0}}} />);
    expect(screen.getByText(/Current task context retained/)).toBeTruthy();
    expect(screen.queryByText(/source-only-in-details/)).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Context selection details"}));
    expect(screen.getAllByText("0 / 0")).toHaveLength(2);
    expect(screen.getByText("history_token_ceiling")).toBeTruthy();
    expect(screen.getByText(/source-only-in-details/)).toBeTruthy();
  });
  it("does not claim retained definitions during recovery or invent missing measurements", () => {
    render(<ContextDiagnostics receipt={{context_projection: {recovery_only: true}}} />);
    expect(screen.getByText(/definitions are being recovered/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button"));
    expect(screen.getAllByText(/Not recorded/).length).toBeGreaterThan(0);
    expect(screen.queryByText(/Current task context retained/)).toBeNull();
  });
});
