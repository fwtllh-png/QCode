import {cleanup, fireEvent, render, screen} from "@testing-library/react";
import {afterEach, describe, expect, it, vi} from "vitest";

import {SessionProgress} from "./SessionProgress";

afterEach(cleanup);

describe("SessionProgress", () => {
  it("renders plan items, subagents, and trajectory navigation", () => {
    const onOpenTrajectory = vi.fn();
    render(
      <SessionProgress
        plan={{
          version: 1,
          id: "plan",
          session_id: "session",
          thread_id: "thread",
          turn_id: "turn",
          cursor: 1,
          status: "ready",
          body: `{"version":1,"revision":2,"objective":"Ship the parser fix",` +
            `"steps":[{"id":"inspect","title":"Inspect parser","status":"done"},` +
            `{"id":"implement","title":"Implement parser","status":"in_progress"},` +
            `{"id":"verify","title":"Verify parser","status":"pending"}]}`,
          document: {
            version: 1,
            revision: 2,
            title: "Parser rollout",
            objective: "Ship the parser fix",
            steps: [
              {id: "inspect", title: "Inspect parser", status: "done"},
              {
                id: "implement",
                title: "Implement parser",
                status: "in_progress",
                expected_evidence: "Focused tests pass"
              },
              {id: "verify", title: "Verify parser", status: "pending"}
            ]
          },
          profile_revision: 1,
          can_implement: true,
          can_autopilot: false,
          created_at: "2026-01-01T00:00:00Z"
        }}
        agents={[
          {
            id: "agent",
            role: "reviewer",
            status: "running",
            last_message: "Checking the diff"
          }
        ]}
        activeTurnID="turn"
        latestTurnID="turn"
        onOpenTrajectory={onOpenTrajectory}
      />
    );

    expect(screen.queryByText("Parser rollout")).toBeNull();
    expect(screen.queryByText("Ship the parser fix")).toBeNull();
    expect(screen.queryByText("Revision 2")).toBeNull();
    expect(screen.getByText("Implement parser")).toBeTruthy();
    expect(screen.getByText("1 completed · 1 active · 1 pending")).toBeTruthy();
    expect(screen.queryByText("Focused tests pass")).toBeNull();
    expect(screen.getByText("Checking the diff")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Collapse subagents"}));
    expect(screen.queryByText("Checking the diff")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Expand subagents"}));
    expect(screen.getByText("Checking the diff")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Collapse plan"}));
    expect(screen.queryByText("Implement parser")).toBeNull();
    expect(screen.getByText("1 completed · 1 active · 1 pending")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Expand plan"}));
    expect(screen.getByText("Implement parser")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Open trajectory"}));
    expect(onOpenTrajectory).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", {name: "Implement"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Autopilot"})).toBeNull();
  });

  it("does not render plan metadata when there are no task items", () => {
    const body = `{"version":1,"revision":1,` +
      `"constraints":["Keep the prefix stable"],"steps":[]}`;
    render(
      <SessionProgress
        plan={{
          version: 1,
          id: "plan",
          session_id: "session",
          thread_id: "thread",
          turn_id: "turn",
          cursor: 1,
          status: "ready",
          body,
          document: {version: 1, revision: 1, steps: []},
          profile_revision: 1,
          can_implement: true,
          can_autopilot: true,
          created_at: "2026-01-01T00:00:00Z"
        }}
        agents={[]}
        activeTurnID=""
        latestTurnID="turn"
        onOpenTrajectory={vi.fn()}
      />
    );

    expect(screen.queryByRole("region", {name: "Session progress"})).toBeNull();
    expect(screen.queryByText("Implementation plan")).toBeNull();
    expect(screen.queryByText(body)).toBeNull();
  });

  it("does not show stale plan work as active after its turn ends", () => {
    render(
      <SessionProgress
        plan={{
          version: 1,
          id: "plan",
          session_id: "session",
          thread_id: "thread",
          turn_id: "finished-turn",
          cursor: 1,
          status: "ready",
          body: "{}",
          document: {
            version: 1,
            revision: 1,
            steps: [
              {id: "done", title: "Done", status: "done"},
              {id: "stale", title: "Interrupted", status: "in_progress"}
            ]
          },
          profile_revision: 1,
          can_implement: true,
          can_autopilot: false,
          created_at: "2026-01-01T00:00:00Z"
        }}
        agents={[]}
        activeTurnID=""
        latestTurnID="finished-turn"
        onOpenTrajectory={vi.fn()}
      />
    );

    expect(screen.getByText("1 completed · 0 active · 1 pending")).toBeTruthy();
    expect(document.querySelector(".spin")).toBeNull();
  });

  it("retires the plan once a later turn starts", () => {
    render(
      <SessionProgress
        plan={{
          version: 1,
          id: "plan",
          session_id: "session",
          thread_id: "thread",
          turn_id: "plan-turn",
          cursor: 1,
          status: "ready",
          body: `{"version":1,"revision":1,"steps":[` +
            `{"id":"one","title":"Outdated step","status":"pending"}]}`,
          document: {
            version: 1,
            revision: 1,
            steps: [{id: "one", title: "Outdated step", status: "pending"}]
          },
          profile_revision: 1,
          can_implement: true,
          can_autopilot: false,
          created_at: "2026-01-01T00:00:00Z"
        }}
        agents={[]}
        activeTurnID="next-turn"
        latestTurnID="next-turn"
        onOpenTrajectory={vi.fn()}
      />
    );

    expect(screen.queryByRole("region", {name: "Session progress"})).toBeNull();
    expect(screen.queryByText("Outdated step")).toBeNull();
    expect(screen.queryByText("Proposed plan")).toBeNull();
  });

  it("keeps active subagents visible while retiring a superseded plan", () => {
    render(
      <SessionProgress
        plan={{
          version: 1,
          id: "plan",
          session_id: "session",
          thread_id: "thread",
          turn_id: "plan-turn",
          cursor: 1,
          status: "ready",
          body: `{"version":1,"revision":1,"steps":[` +
            `{"id":"one","title":"Outdated step","status":"pending"}]}`,
          document: {
            version: 1,
            revision: 1,
            steps: [{id: "one", title: "Outdated step", status: "pending"}]
          },
          profile_revision: 1,
          can_implement: false,
          can_autopilot: false,
          created_at: "2026-01-01T00:00:00Z"
        }}
        agents={[
          {
            id: "agent",
            role: "reviewer",
            status: "running",
            last_message: "Still reviewing"
          }
        ]}
        activeTurnID="next-turn"
        latestTurnID="next-turn"
        onOpenTrajectory={vi.fn()}
      />
    );

    expect(screen.queryByText("Outdated step")).toBeNull();
    expect(screen.queryByText("Tasks")).toBeNull();
    expect(screen.getByText("Still reviewing")).toBeTruthy();
  });

  it("removes settled subagents from progress after a failed turn is retried", () => {
    render(
      <SessionProgress
        agents={[
          {
            id: "agent-old-1",
            role: "review",
            status: "failed",
            last_message: "provider rate limit retry budget exhausted"
          },
          {
            id: "agent-old-2",
            role: "review",
            status: "completed",
            last_message: "Completed before retry"
          }
        ]}
        activeTurnID="retried-turn"
        latestTurnID="retried-turn"
        onOpenTrajectory={vi.fn()}
      />
    );

    expect(screen.queryByRole("region", {name: "Session progress"})).toBeNull();
    expect(screen.queryByText(/rate limit retry budget exhausted/)).toBeNull();
  });
});
