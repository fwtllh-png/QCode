import {readFileSync} from "node:fs";
import {resolve} from "node:path";
import {describe, expect, it} from "vitest";
import {reconnectBaseDelayMs, reconnectDelay, reconnectMaxDelayMs} from "./reconnect";
import {ScopeSlot} from "./requestScope";

describe("reconnectDelay", () => {
  it("starts at the base delay and doubles per consecutive failure", () => {
    expect(reconnectDelay(0)).toBe(reconnectBaseDelayMs);
    expect(reconnectDelay(1)).toBe(reconnectBaseDelayMs * 2);
    expect(reconnectDelay(2)).toBe(reconnectBaseDelayMs * 4);
  });

  it("never exceeds the cap", () => {
    expect(reconnectDelay(5)).toBe(reconnectBaseDelayMs * 32);
    expect(reconnectDelay(6)).toBe(reconnectMaxDelayMs);
    expect(reconnectDelay(10_000)).toBe(reconnectMaxDelayMs);
    expect(reconnectDelay(Number.POSITIVE_INFINITY)).toBe(reconnectMaxDelayMs);
  });

  it("treats invalid failure counts as the first attempt", () => {
    expect(reconnectDelay(-3)).toBe(reconnectBaseDelayMs);
    expect(reconnectDelay(Number.NaN)).toBe(reconnectBaseDelayMs);
  });
});

describe("RuntimeClient request scopes", () => {
  it("guards late results with scopes, not generation counters", () => {
    const source = readFileSync(resolve(process.cwd(), "src/runtime/client.ts"), "utf8");

    expect(source).not.toMatch(/\bgeneration\b|Generation\b/);
  });
});

describe("ScopeSlot", () => {
  it("ends the previous scope when renewed", () => {
    const slot = new ScopeSlot();
    const first = slot.current;
    const second = slot.renew();

    expect(first.live).toBe(false);
    expect(first.signal.aborted).toBe(true);
    expect(second.live).toBe(true);
    expect(slot.current).toBe(second);
  });
});
