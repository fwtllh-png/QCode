export function normalizeApprovalPosture(value: string): string {
  return value === "suggest" ? "auto" : value;
}

export function approvalPostureLabel(value: string): string {
  switch (normalizeApprovalPosture(value)) {
    case "auto": return "Auto";
    case "never": return "Read only";
    case "bypass": return "Full Access";
    default: return value;
  }
}

export const approvalPostureDescription =
  "Auto runs routine actions and asks when needed. Full Access lets commands change host files and access the network without routine approval. Protected paths and explicit rules still apply. Read only prevents changes.";

export const approvalPostures = ["never", "auto", "bypass"];
