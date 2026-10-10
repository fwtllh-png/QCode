export interface GuardianReview {
  readonly reviewID: string;
  readonly sequence: number;
  readonly phase: string;
  readonly reasonCode: string;
  readonly provider: string;
  readonly model: string;
  readonly risk: string;
  readonly authorization: string;
  readonly recommendation: string;
  readonly action: string;
  readonly authority: string;
  readonly policyCode: string;
  readonly approvalRequestID: string;
  readonly inputTokens?: number;
  readonly outputTokens?: number;
}

const object = (value: unknown): Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : {};
const text = (value: unknown): string => typeof value === "string" ? value : "";

export function projectGuardian(data: Record<string, unknown>, sequence: number): GuardianReview | undefined {
  if (!text(data.review_id) || !text(data.call_id)) return undefined;
  const assessment = object(data.assessment);
  const decision = object(data.decision);
  const usage = object(data.usage);
  return {
    reviewID: text(data.review_id), sequence,
    phase: text(data.phase), reasonCode: text(data.reason_code),
    provider: text(data.provider), model: text(data.model),
    risk: text(assessment.risk), authorization: text(assessment.authorization),
    recommendation: text(assessment.recommendation), action: text(decision.action),
    authority: text(decision.authority), policyCode: text(decision.code),
    approvalRequestID: text(data.approval_request_id),
    inputTokens: typeof usage.input_tokens === "number" ? usage.input_tokens : undefined,
    outputTokens: typeof usage.output_tokens === "number" ? usage.output_tokens : undefined
  };
}

export function guardianReason(code: unknown): string {
  switch (code) {
    case "review_started": return "Automatic review in progress.";
    case "assessment_complete": return "Assessment recorded; authorization is checked separately.";
    case "approval_required": return "Automatic review requires your confirmation.";
    case "evidence_invalidated": return "The reviewed evidence changed. Please confirm the current operation.";
    case "execution_invalidated": return "Authorization changed before execution. The reviewed operation was not started.";
    case "review_canceled": return "Automatic review was canceled.";
    case "human_approved": return "User approval recorded.";
    case "human_denied": return "User declined this operation.";
    case "human_canceled": return "Approval was canceled.";
    case "approval_expired": return "Approval expired.";
    case "automatic_approval_disabled": return "Automatic approval is disabled. Please confirm this operation.";
    case "policy_decision": return "Current policy decision recorded.";
    default: return "Automatic review could not complete. Your confirmation is required.";
  }
}
