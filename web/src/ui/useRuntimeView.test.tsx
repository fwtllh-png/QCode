import {act, render, screen} from "@testing-library/react";
import {describe, expect, it} from "vitest";
import {ConversationProjection, emptyConversationSnapshot} from "../projection/conversation";
import type {RuntimeEvent} from "../protocol";
import type {RuntimeSnapshot} from "../runtime/client";
import {isContentFrame, useLiveNode, useWorkbenchSnapshot} from "./useRuntimeView";

describe("useWorkbenchSnapshot", () => {
  it("skips streaming frames while the node renderer follows them", () => {
    const store = new FakeStore();
    store.push(event(1, "turn.started", {prompt: "Explain"}));
    store.push(event(2, "output.delta", {text: "Hel"}));
    let workbenchRenders = 0;
    function Workbench() {
      workbenchRenders += 1;
      const snapshot = useWorkbenchSnapshot(store);
      const node = snapshot.conversation.nodes.get("output-turn");
      return node?.kind === "assistant" ? <Answer entry={node} /> : null;
    }
    function Answer({entry}: {entry: Extract<ReturnType<FakeStore["node"]>, {kind: "assistant"}>}) {
      const live = useLiveNode(store, entry);
      return <p>{live.text}</p>;
    }
    render(<Workbench />);
    const initial = workbenchRenders;

    act(() => store.push(event(3, "output.delta", {text: "lo"})));
    expect(screen.getByText("Hello")).toBeTruthy();
    expect(workbenchRenders).toBe(initial);

    act(() => store.push(event(4, "turn.completed", {})));
    expect(workbenchRenders).toBe(initial + 1);
  });
});

describe("isContentFrame", () => {
  it("treats any non-content change as a workbench frame", () => {
    const store = new FakeStore();
    store.push(event(1, "turn.started", {prompt: "Explain"}));
    store.push(event(2, "output.delta", {text: "a"}));
    const base = store.getSnapshot();
    store.push(event(3, "output.delta", {text: "b"}));
    const streamed = store.getSnapshot();

    expect(isContentFrame(base, streamed)).toBe(true);
    expect(isContentFrame(base, {...streamed, selectedSessionID: "other"})).toBe(false);
    expect(isContentFrame(base, {...streamed, events: streamed.events.slice(1)})).toBe(false);
    store.push(event(4, "turn.usage", {}));
    expect(isContentFrame(streamed, store.getSnapshot())).toBe(false);
  });
});

class FakeStore {
  private readonly projection = new ConversationProjection();
  private readonly listeners = new Set<() => void>();
  private state = {
    events: [],
    conversation: emptyConversationSnapshot(),
    selectedSessionID: "session"
  } as unknown as RuntimeSnapshot;

  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = () => this.state;

  node() {
    return this.state.conversation.nodes.get("output-turn")!;
  }

  push(value: RuntimeEvent): void {
    this.projection.apply(value);
    this.state = {
      ...this.state,
      events: [...this.state.events, value],
      conversation: this.projection.snapshot()
    };
    this.listeners.forEach((listener) => listener());
  }
}

function event(sequence: number, kind: string, data: Record<string, unknown>): RuntimeEvent {
  return {
    version: 1,
    id: `event-${sequence}`,
    kind,
    operation_id: "operation",
    thread_id: "thread",
    turn_id: "turn",
    item_id: `item-${sequence}`,
    sequence,
    created_at: "2026-01-01T00:00:00Z",
    data
  };
}
