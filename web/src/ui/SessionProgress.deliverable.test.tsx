import {cleanup, render, screen} from "@testing-library/react";
import {afterEach, expect, it, vi} from "vitest";

import {SessionProgress} from "./SessionProgress";

afterEach(cleanup);

it("shows a deliverable as a proposal rather than running tasks", () => {
  render(
    <SessionProgress
      plan={{
        version: 2,
        id: "plan",
        session_id: "session",
        thread_id: "thread",
        turn_id: "turn",
        cursor: 1,
        status: "ready",
        purpose: "deliverable",
        body: "{}",
        document: {
          version: 1,
          purpose: "deliverable",
          steps: [
            {id: "future", title: "Future implementation", status: "in_progress"}
          ]
        },
        profile_revision: 1,
        can_implement: false,
        can_autopilot: false,
        created_at: "2026-01-01T00:00:00Z"
      }}
      agents={[]}
      activeTurnID="turn"
      latestTurnID="turn"
      onOpenTrajectory={vi.fn()}
    />
  );

  expect(screen.getByText("Proposed plan")).toBeTruthy();
  expect(screen.getByText("Future implementation")).toBeTruthy();
  expect(screen.queryByText("Tasks")).toBeNull();
  expect(screen.queryByText(/active.*pending/)).toBeNull();
  expect(document.querySelector(".spin")).toBeNull();
});
