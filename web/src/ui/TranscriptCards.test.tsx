import {cleanup, fireEvent, render, screen} from "@testing-library/react";
import {afterEach, describe, expect, it, vi} from "vitest";

import type {ConversationNode} from "../projection/conversation";
import {AgentDisclosure} from "./TranscriptCards";

afterEach(cleanup);

describe("AgentDisclosure", () => {
  it("shows a running subagent execution and opens tool inspection", () => {
    const onInspect = vi.fn();
    const entry: Extract<ConversationNode, {kind: "agent"}> = {
      id: "agent-agent-1",
      kind: "agent",
      turnID: "turn_external",
      sequence: 1,
      agentID: "agent-1",
      role: "review",
      taskName: "Review persistence",
      status: "running",
      summary: "Reading the history service",
      state: "running",
      activities: [{
        id: "activity-1",
        sequence: 2,
        kind: "tool",
        title: "Read",
        summary: "internal/persist/history/service.go",
        state: "running",
        callID: "call-1"
      }]
    };

    render(<AgentDisclosure entry={entry} onInspect={onInspect} />);

    // 运行中的 agent 卡默认折叠（避免高度变化推移视口），手动展开后可用。
    const row = screen.getByRole("button", {name: /Review · agent-1/});
    expect(row.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(row);
    expect(row.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("internal/persist/history/service.go")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Inspect Read"}));
    expect(onInspect).toHaveBeenCalledWith("call-1");
  });

  it("opens failed cards and shows reason plus usage", () => {
    const entry: Extract<ConversationNode, {kind: "agent"}> = {
      id: "agent-agent-2",
      kind: "agent",
      turnID: "turn_external",
      sequence: 2,
      agentID: "agent-2",
      role: "review",
      taskName: "Audit paxos",
      status: "failed",
      summary: "token budget exhausted: projected 17698, limit 15000",
      reasonCode: "budget_exhausted",
      usage: {inputTokens: 12817, outputTokens: 146},
      state: "failed",
      activities: []
    };

    render(<AgentDisclosure entry={entry} onInspect={() => undefined} />);

    expect(screen.getByRole("button", {name: /Review · agent-2/})
      .getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText(/budget exhausted/)).toBeTruthy();
    expect(screen.getByText(/in 12817 \/ out 146/)).toBeTruthy();
  });
});
