import {
  Bot,
  CheckCircle2,
  ChevronDown,
  CircleDashed,
  ListChecks,
  LoaderCircle
} from "lucide-react";
import {useState} from "react";

import type {
  AgentSummary,
  SessionPlanArtifact
} from "../protocol";

export function SessionProgress({
  plan,
  agents,
  activeTurnID,
  latestTurnID,
  onOpenTrajectory
}: {
  plan?: SessionPlanArtifact;
  agents: readonly AgentSummary[];
  activeTurnID: string;
  latestTurnID: string;
  onOpenTrajectory: () => void;
}) {
  const [planExpanded, setPlanExpanded] = useState(true);
  // A plan belongs to the turn that produced it. Keep it on screen while that
  // turn is the newest one — including after it settles, so a proposal can be
  // reviewed — but retire it once a later turn starts.
  const planSuperseded = Boolean(
    plan?.turn_id && latestTurnID && plan.turn_id !== latestTurnID
  );
  const isDeliverable = plan?.purpose === "deliverable" ||
    plan?.document?.purpose === "deliverable";
  const planSteps = plan?.document?.steps ?? [];
  const planDone = planSteps.filter((step) => step.status === "done").length;
  const planActive = !isDeliverable && plan?.turn_id === activeTurnID ? planSteps.filter(
    (step) => step.status === "in_progress"
  ).length : 0;
  const planPending = planSteps.length - planDone - planActive;
  const hasPlanContent = !planSuperseded && Boolean(plan?.document?.steps.length);
  const activeAgents = agents.filter(isActiveAgent);
  const [agentsExpanded, setAgentsExpanded] = useState(true);
  if (!hasPlanContent && activeAgents.length === 0) return null;

  return (
    <section className="sessionProgress" aria-label="Session progress">
      <div className="sessionProgressBody">
        {hasPlanContent && plan && (
          <div
            className="progressSection progressPlan"
            data-collapsed={planExpanded ? undefined : true}
          >
            <button
              type="button"
              className="planDisclosure"
              aria-label={planExpanded ? "Collapse plan" : "Expand plan"}
              aria-expanded={planExpanded}
              title={planExpanded ? "Collapse plan" : "Expand plan"}
              onClick={() => setPlanExpanded((value) => !value)}
            >
              <span className="planSummary">
                <ListChecks size={15} />
                <strong>{isDeliverable ? "Proposed plan" : "Tasks"}</strong>
                <small>
                  {isDeliverable
                    ? `${planSteps.length} steps`
                    : `${planDone} completed · ${planActive} active · ${planPending} pending`}
                </small>
              </span>
              <ChevronDown size={15} data-expanded={planExpanded || undefined} />
            </button>
            {planExpanded && plan.document && (
              <ol className="progressTasks">
                {plan.document.steps.map((step) => (
                  <li
                    key={step.id}
                    data-state={step.status === "in_progress" && planActive === 0
                      ? "pending"
                      : step.status}
                  >
                    {step.status === "done"
                      ? <CheckCircle2 size={16} />
                      : step.status === "in_progress" && planActive > 0
                        ? <LoaderCircle className="spin" size={16} />
                        : <CircleDashed size={16} />}
                    <strong>{step.title}</strong>
                  </li>
                ))}
              </ol>
            )}
          </div>
        )}
        {activeAgents.length > 0 && (
          <div
            className="progressSection progressAgentsSection"
            data-collapsed={agentsExpanded ? undefined : true}
          >
            <div className="progressAgentsHeader">
              <button
                type="button"
                className="progressAgentDisclosure"
                aria-label={agentsExpanded ? "Collapse subagents" : "Expand subagents"}
                aria-expanded={agentsExpanded}
                title={agentsExpanded ? "Collapse subagents" : "Expand subagents"}
                onClick={() => setAgentsExpanded((value) => !value)}
              >
                <span className="agentSummary">
                  <Bot size={14} />
                  <strong>Subagents</strong>
                  <small>{activeAgents.length} active</small>
                </span>
                <ChevronDown size={15} data-expanded={agentsExpanded || undefined} />
              </button>
              <button
                type="button"
                className="progressTrajectory"
                onClick={onOpenTrajectory}
              >
                Open trajectory
              </button>
            </div>
            {agentsExpanded && (
              <ul className="progressAgents">
                {activeAgents.map((agent) => (
                  <li key={agent.id}>
                    <span data-status={agent.status} />
                    <strong>{agent.role}</strong>
                    <b>{agent.status.replaceAll("_", " ")}</b>
                    {agent.last_message && <small>{agent.last_message}</small>}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    </section>
  );
}

function isActiveAgent(agent: AgentSummary): boolean {
  switch (agent.status) {
    case "completed":
    case "failed":
    case "interrupted":
    case "integrated":
    case "integration_failed":
    case "closed":
      return false;
    default:
      return true;
  }
}
